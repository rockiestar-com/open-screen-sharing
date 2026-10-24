#if os(iOS)
import UIKit
import UserNotifications

final class ScreenWindowTracker {
    private unowned let view: UIView
    private let report: (_ area: CGSize, _ scale: CGFloat, _ screen: CGSize) -> Void
    private weak var scene: UIWindowScene?
    private var geometryObservation: NSKeyValueObservation?
    private var reportTask: Task<Void, Never>?
    private var reportedArea: CGSize?
    private var reportedScale: CGFloat?
    private var matchesWindow = false
    private var replacedMinimumSize: CGSize?
    private var turnsLandscape = true
    private var preferredOrientation: UIInterfaceOrientationMask?
    private var awaitsRotation = false

    init(view: UIView, report: @escaping (_ area: CGSize, _ scale: CGFloat, _ screen: CGSize) -> Void) {
        self.view = view
        self.report = report
    }

    var screenArea: CGRect? {
        guard let window = view.window else { return nil }
        var area = window.bounds
        if view.traitCollection.userInterfaceIdiom == .phone {
            let safeArea = window.safeAreaInsets
            var insets = UIEdgeInsets.zero
            switch window.windowScene?.interfaceOrientationCompat {
            case .landscapeLeft, .landscapeRight:
                insets.left = safeArea.left
                insets.right = safeArea.right
            case .portraitUpsideDown: insets.bottom = safeArea.bottom
            default: insets.top = safeArea.top
            }
            area = area.inset(by: insets)
        }
        let visible = view.convert(area, from: window).intersection(view.bounds)
        return visible.isNull || visible.isEmpty ? nil : visible
    }

    func windowDidChange() {
        let newScene = view.window?.windowScene
        if newScene !== scene {
            detach()
            scene = newScene
            if let newScene {
                if #available(iOS 16, *) {
                    geometryObservation = newScene.observe(\.effectiveGeometry) { [weak self] _, _ in
                        MainActor.assumeIsolated { self?.geometryDidChange() }
                    }
                }
                applySizeRestriction()
            }
        }
        geometryDidChange()
    }

    func geometryDidChange() {
        guard let scene, let area = screenArea?.size, area.width > 0, area.height > 0 else { return }
        updateOrientation(scene)
        let awaitsOrientation =
            view.traitCollection.userInterfaceIdiom == .phone && matchesWindow && preferredOrientation == nil
        guard !awaitsRotation, !awaitsOrientation, !scene.isInteractivelyResizingCompat else {
            reportTask?.cancel()
            reportTask = nil
            return
        }
        guard reportTask == nil else { return }
        reportTask = Task { [weak self] in
            guard let self, !Task.isCancelled else { return }
            self.reportTask = nil
            self.reportArea()
        }
    }

    func setTurnsLandscape(_ turns: Bool) {
        guard turns != turnsLandscape else { return }
        turnsLandscape = turns
        geometryDidChange()
    }

    func setPreferredOrientation(_ orientation: UIInterfaceOrientationMask?) {
        guard orientation != preferredOrientation else { return }
        preferredOrientation = orientation
        geometryDidChange()
    }

    func setMatchesWindow(_ matches: Bool) {
        guard matches != matchesWindow else { return }
        matchesWindow = matches
        applySizeRestriction()
        geometryDidChange()
    }

    func detach() {
        geometryObservation = nil
        reportTask?.cancel()
        reportTask = nil
        reportedArea = nil
        reportedScale = nil
        awaitsRotation = false
        guard let scene else { return }
        if let replacedMinimumSize {
            scene.sizeRestrictions?.minimumSize = replacedMinimumSize
            self.replacedMinimumSize = nil
        }
        SessionOrientation.update(scene, to: nil)
        self.scene = nil
    }

    private func reportArea() {
        guard let scene, !scene.isInteractivelyResizingCompat, let area = screenArea?.size else { return }
        let scale = view.traitCollection.displayScale
        guard area != reportedArea || scale != reportedScale else { return }
        reportedArea = area
        reportedScale = scale
        report(area, scale, scene.screen.bounds.size)
    }

    private func applySizeRestriction() {
        guard let restrictions = scene?.sizeRestrictions else { return }
        if matchesWindow {
            if replacedMinimumSize == nil {
                replacedMinimumSize = restrictions.minimumSize
            }
            restrictions.minimumSize = VirtualDisplayMode.minimumWindowPoints
        } else if let replacedMinimumSize {
            restrictions.minimumSize = replacedMinimumSize
            self.replacedMinimumSize = nil
        }
    }

    private func updateOrientation(_ scene: UIWindowScene) {
        guard let window = view.window else { return }
        let orientation: UIInterfaceOrientationMask?
        if view.traitCollection.userInterfaceIdiom == .phone {
            orientation = preferredOrientation
        } else if turnsLandscape {
            let screen = scene.screen.bounds.size
            let size = window.bounds.size
            let fillsScreen =
                (abs(size.width - screen.width) < 1 && abs(size.height - screen.height) < 1)
                || (abs(size.width - screen.height) < 1 && abs(size.height - screen.width) < 1)
            orientation = fillsScreen ? .landscape : nil
        } else {
            orientation = nil
        }
        let requested = SessionOrientation.update(scene, to: orientation) { [weak self] in
            guard let self, self.awaitsRotation else { return }
            self.awaitsRotation = false
            self.geometryDidChange()
        }
        let current = UIInterfaceOrientationMask(rawValue: 1 << scene.interfaceOrientationCompat.rawValue)
        awaitsRotation = (requested || awaitsRotation) && orientation.map { !$0.contains(current) } ?? false
    }
}

enum SessionOrientation {
    private static let lockedScenes = NSMapTable<UIWindowScene, NSNumber>.weakToStrongObjects()

    static func supportedOrientations(for scene: UIWindowScene?) -> UIInterfaceOrientationMask {
        if let scene, let locked = lockedScenes.object(forKey: scene) {
            return UIInterfaceOrientationMask(rawValue: locked.uintValue)
        }
        return UIDevice.current.userInterfaceIdiom == .pad ? .all : .allButUpsideDown
    }

    @discardableResult
    static func update(
        _ scene: UIWindowScene, to orientation: UIInterfaceOrientationMask?, refused: @escaping @MainActor @Sendable () -> Void = {}
    ) -> Bool {
        guard lockedScenes.object(forKey: scene)?.uintValue != orientation?.rawValue else { return false }
        if let orientation {
            lockedScenes.setObject(NSNumber(value: orientation.rawValue), forKey: scene)
        } else {
            lockedScenes.removeObject(forKey: scene)
        }
        guard #available(iOS 16, *) else {
            UIViewController.attemptRotationToDeviceOrientation()
            return false
        }
        for window in scene.windows {
            var controller = window.rootViewController
            while let current = controller {
                current.setNeedsUpdateOfSupportedInterfaceOrientations()
                controller = current.presentedViewController
            }
        }
        guard let orientation else { return false }
        scene.requestGeometryUpdate(.iOS(interfaceOrientations: orientation)) { _ in
            Task { @MainActor in refused() }
        }
        return true
    }
}

extension UIWindowScene {
    fileprivate var interfaceOrientationCompat: UIInterfaceOrientation {
        if #available(iOS 16, *) {
            effectiveGeometry.interfaceOrientation
        } else {
            interfaceOrientation
        }
    }

    fileprivate var isInteractivelyResizingCompat: Bool {
        if #available(iOS 26, *) {
            effectiveGeometry.isInteractivelyResizing
        } else {
            false
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        #if DEBUG || SIDELOAD
        UNUserNotificationCenter.current().delegate = self
        #endif
        return true
    }

    func application(_ application: UIApplication, supportedInterfaceOrientationsFor window: UIWindow?) -> UIInterfaceOrientationMask {
        SessionOrientation.supportedOrientations(for: window?.windowScene)
    }
}

#if DEBUG || SIDELOAD
extension AppDelegate: UNUserNotificationCenterDelegate {
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list, .sound]
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        if response.notification.request.identifier == ServerController.problemNotification {
            HomeRequest.shared.pending = .server
        }
    }
}
#endif
#endif

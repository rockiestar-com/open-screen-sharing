import Foundation
import Libsharing
import Perception
import UIKit
import UserNotifications
import dnssd
import os

@Perceptible
final class ServerController {
    static let shared = ServerController()
    nonisolated private static let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "OpenScreenSharing", category: "server")
    nonisolated static let problemNotification = "server-problem"

    enum Phase: Equatable {
        case stopped
        case starting
        case connecting
        case preparing
        case running
        case failed(ServerProblem)

        var isActive: Bool {
            switch self {
            case .stopped, .failed: false
            case .starting, .connecting, .preparing, .running: true
            }
        }

        var isRunning: Bool {
            if case .running = self { true } else { false }
        }
    }

    private(set) var phase: Phase = .stopped {
        didSet {
            guard phase != oldValue else { return }
            phaseChanged()
        }
    }

    private(set) var advertisedName: String?
    private(set) var localNetworkDenied = false
    private(set) var tunnelFound: Bool?
    private(set) var tunnelThroughLocalNetwork = false

    let remotePairing = RemotePairingBrowser()
    let pairingSession = PairingSession()
    @PerceptionIgnored private lazy var location: BackgroundLocation = {
        let location = BackgroundLocation()
        location.onUpdatingChange = { [weak self] in
            self?.updateIdleTimer()
        }
        location.onDenied = { [weak self] in
            self?.fail(ServerProblem(kind: .locationDenied, message: ""))
        }
        return location
    }()
    @PerceptionIgnored private lazy var audio: BackgroundAudio = {
        let audio = BackgroundAudio()
        audio.onPlayingChange = { [weak self] in
            self?.updateIdleTimer()
        }
        audio.onFailure = { [weak self] error in
            self?.fail(ServerProblem(kind: .other, message: error.localizedDescription))
        }
        return audio
    }()

    @PerceptionIgnored private lazy var volume = LocalVolume()
    @PerceptionIgnored private lazy var brightness = LocalBrightness(queue: Self.queue)
    @PerceptionIgnored private var viewers: Set<Int64> = []

    @PerceptionIgnored private var server: LibsharingDeviceServer?
    @PerceptionIgnored private var generation = 0
    @PerceptionIgnored private var startTask: Task<Void, Never>?
    @PerceptionIgnored private var closingTask: Task<Void, Never>?
    @PerceptionIgnored private var cleanupBackgroundTask: UIBackgroundTaskIdentifier = .invalid
    @PerceptionIgnored private var probeTask: Task<Void, Never>?
    @PerceptionIgnored private var isBrowsing = false
    private let advertiser = BonjourAdvertiser()
    nonisolated private static let queue = DispatchQueue(label: "io.nekohasekai.screensharing.server", qos: .userInitiated)

    private var settings: ServerSettings { .shared }
    private var pairing: PairingStore { .shared }

    init() {
        remotePairing.onPortChange = { [weak self] port in
            self?.server?.setRemotePairingPort(Int32(port))
        }
        advertiser.onRegister = { [weak self] name in
            self?.advertisedName = name
        }
        advertiser.onFailure = { [weak self] errorCode in
            self?.advertisingFailed(errorCode)
        }
    }

    var isActive: Bool { phase.isActive }

    var canStart: Bool {
        pairing.isPaired && settings.isComplete && tunnelFound != false && !DiskImageStore.shared.isImporting
    }

    func startProbing() {
        guard probeTask == nil else { return }
        probeTask = Task {
            while !Task.isCancelled {
                let throughLocalNetwork: Bool
                if phase.isActive {
                    throughLocalNetwork = server?.loopbackAddress()?.localNetwork ?? false
                } else {
                    let port = remotePairing.port ?? 0
                    let probe = UncheckedSendable(value: LibsharingNewLoopbackProbe(Int32(port))!)
                    let result = await withTaskCancellationHandler {
                        await Task.detached(priority: .utility) {
                            do {
                                let loopback = try probe.value.run()
                                return (found: true, localNetwork: loopback.localNetwork)
                            } catch {
                                return (found: false, localNetwork: false)
                            }
                        }.value
                    } onCancel: {
                        probe.value.cancel()
                    }
                    guard !Task.isCancelled else { return }
                    if tunnelFound != result.found {
                        tunnelFound = result.found
                    }
                    throughLocalNetwork = result.localNetwork
                }
                if tunnelThroughLocalNetwork != throughLocalNetwork {
                    tunnelThroughLocalNetwork = throughLocalNetwork
                }
                try? await Task.sleep(seconds: 2)
            }
        }
    }

    func stopProbing() {
        probeTask?.cancel()
        probeTask = nil
    }

    private var name: String {
        if let name = pairing.device?.name, !name.isEmpty {
            return name
        }
        return UIDevice.current.name
    }

    func resume() {
        volume.setWatched(false)
        guard settings.isEnabled else { return }
        start()
    }

    func start() {
        guard !phase.isActive, canStart, let record = pairing.record else { return }
        if settings.runsInBackground, settings.backgroundMode == .location, location.isDenied {
            fail(ServerProblem(kind: .locationDenied, message: ""))
            return
        }
        settings.isEnabled = true
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, error in
            if let error {
                Self.logger.error("Request notification authorization: \(error.localizedDescription, privacy: .public)")
            }
        }
        generation += 1
        let generation = generation
        phase = .starting
        startTask = Task {
            guard generation == self.generation else { return }
            await launch(record: record, generation: generation)
        }
    }

    func stop() {
        settings.isEnabled = false
        guard phase != .stopped else { return }
        generation += 1
        startTask?.cancel()
        startTask = nil
        closeServer()
        phase = .stopped
    }

    private func launch(record: Data, generation: Int) async {
        await closingTask?.value
        guard generation == self.generation else { return }
        let options = LibsharingDeviceServerOptions()
        options.pairingRecord = record
        options.remotePairingPort = Int32(remotePairing.port ?? 0)
        options.port = Int32(settings.port)
        options.noAuthentication = !settings.authenticates
        options.username = settings.effectiveUsername
        options.password = settings.password
        options.autoUnlockPasscode = settings.autoUnlocks ? settings.autoUnlockPasscode : ""
        options.autoUnlockPasscodeType = settings.autoUnlockPasscodeType.rawValue
        do {
            options.diskImageDirectory = try DiskImageStore.directory.get().path
        } catch {
            fail(ServerProblem(kind: .diskImage, message: error.localizedDescription))
            return
        }
        let events = DeviceServerEvents(
            state: { [weak self] change in
                DispatchQueue.main.async { self?.handle(change, generation: generation) }
            },
            viewer: { [weak self] viewerID, connected in
                DispatchQueue.main.async {
                    guard let self, generation == self.generation else { return }
                    if connected {
                        self.viewers.insert(viewerID)
                    } else {
                        self.viewers.remove(viewerID)
                    }
                    self.volume.setWatched(!self.viewers.isEmpty)
                    self.brightness.setWatched(!self.viewers.isEmpty, server: self.server)
                }
            })
        let request = UncheckedSendable(value: options)
        let result = await withCheckedContinuation { continuation in
            Self.queue.async {
                continuation.resume(returning: Self.makeServer(request.value, events: events))
            }
        }
        switch result {
        case .success(let box):
            guard generation == self.generation else {
                Self.queue.async {
                    do { try box.value.close() }
                    catch { Self.logger.error("Close unused server: \(error.localizedDescription, privacy: .public)") }
                }
                return
            }
            if let port = remotePairing.port, Int32(port) != options.remotePairingPort {
                box.value.setRemotePairingPort(Int32(port))
            }
            do {
                try box.value.start()
            } catch {
                Self.queue.async {
                    do { try box.value.close() }
                    catch { Self.logger.error("Close unused server: \(error.localizedDescription, privacy: .public)") }
                }
                fail(ServerProblem(kind: .other, message: error.localizedDescription))
                return
            }
            server = box.value
        case .failure(let error):
            guard generation == self.generation else { return }
            fail(ServerProblem(kind: .other, message: error.message))
        }
    }

    nonisolated private static func makeServer(
        _ options: LibsharingDeviceServerOptions, events: DeviceServerEvents
    ) -> Result<UncheckedSendable<LibsharingDeviceServer>, MessageError> {
        LibsharingLog.installed
        var error: NSError?
        guard let server = LibsharingNewDeviceServer(options, events, &error) else {
            return .failure(MessageError(error?.localizedDescription ?? "libsharing returned no server"))
        }
        return .success(UncheckedSendable(value: server))
    }

    private func handle(_ change: DeviceServerStateChange, generation: Int) {
        guard generation == self.generation else { return }
        let problem = change.errorCode == LibsharingDeviceErrorNone ? nil : ServerProblem(code: change.errorCode, message: change.message)
        switch change.state {
        case LibsharingDeviceServerConnecting:
            phase = .connecting
        case LibsharingDeviceServerPreparing:
            phase = .preparing
        case LibsharingDeviceServerRunning:
            phase = .running
        case LibsharingDeviceServerFailed:
            fail(problem ?? ServerProblem(kind: .other, message: change.message))
        case LibsharingDeviceServerStopped:
            brightness.setWatched(false, server: nil)
            self.generation += 1
            server = nil
            settings.isEnabled = false
            phase = problem.map(Phase.failed) ?? .stopped
        default:
            break
        }
    }

    private func fail(_ problem: ServerProblem) {
        settings.isEnabled = false
        generation += 1
        startTask?.cancel()
        startTask = nil
        closeServer()
        phase = .failed(problem)
    }

    private func closeServer() {
        guard let server else { return }
        self.server = nil
        cleanupBackgroundTask = UIApplication.shared.beginBackgroundTask(withName: "Restore local brightness") { [weak self] in
            guard let self else { return }
            brightness.setWatched(false, server: nil)
            endCleanupBackgroundTask()
        }
        let restoration = brightness.stop(server)
        let box = UncheckedSendable(value: server)
        let generation = generation
        closingTask = Task {
            defer { endCleanupBackgroundTask() }
            await restoration.value
            let error: String? = await withCheckedContinuation { continuation in
                Self.queue.async {
                    do {
                        try box.value.close()
                        continuation.resume(returning: nil)
                    } catch {
                        continuation.resume(returning: error.localizedDescription)
                    }
                }
            }
            if let error {
                Self.logger.error("Close server: \(error, privacy: .public)")
                if generation == self.generation, self.phase == .stopped {
                    self.phase = .failed(ServerProblem(kind: .other, message: error))
                }
            }
        }
    }

    private func endCleanupBackgroundTask() {
        guard cleanupBackgroundTask != .invalid else { return }
        UIApplication.shared.endBackgroundTask(cleanupBackgroundTask)
        cleanupBackgroundTask = .invalid
    }

    private func phaseChanged() {
        let notifications = UNUserNotificationCenter.current()
        if case .failed(let problem) = phase {
            let content = UNMutableNotificationContent()
            content.title = problem.title
            content.body = problem.message
            content.sound = .default
            notifications.add(UNNotificationRequest(identifier: Self.problemNotification, content: content, trigger: nil)) { error in
                if let error {
                    Self.logger.error("Post the sharing problem: \(error.localizedDescription, privacy: .public)")
                }
            }
        } else {
            notifications.removeDeliveredNotifications(withIdentifiers: [Self.problemNotification])
        }
        if !phase.isActive {
            viewers.removeAll()
            volume.setWatched(false)
        } else if phase.isRunning, viewers.isEmpty, let server {
            brightness.restore(server)
        }
        updateBackground()
        setBrowsing(phase.isActive)
        updateAdvertisement()
    }

    func updateBackground() {
        let mode = phase.isActive && settings.runsInBackground ? settings.backgroundMode : nil
        if mode == .location {
            location.start()
        } else {
            location.stop()
        }
        if mode == .audio {
            audio.start()
        } else {
            audio.stop()
        }
        updateIdleTimer()
    }

    var keepsAppRunning: Bool {
        phase.isActive && (location.isUpdating || audio.isPlaying)
    }

    private func updateIdleTimer() {
        SessionActivity.shared.set(self, active: phase.isActive && !keepsAppRunning)
    }

    private func setBrowsing(_ browsing: Bool) {
        guard browsing != isBrowsing else { return }
        isBrowsing = browsing
        if browsing {
            remotePairing.start()
        } else {
            remotePairing.stop()
        }
    }

    private func updateAdvertisement() {
        guard phase.isRunning else {
            advertiser.stop()
            advertisedName = nil
            localNetworkDenied = false
            return
        }
        let errorCode = advertiser.start(name: name, type: "_rfb._tcp", port: settings.port, txt: ["model": Self.model])
        if errorCode != kDNSServiceErr_NoError {
            advertisingFailed(errorCode)
        }
    }

    private func advertisingFailed(_ errorCode: DNSServiceErrorType) {
        advertisedName = nil
        localNetworkDenied = errorCode == kDNSServiceErr_PolicyDenied
        Self.logger.error("DNSServiceRegister: error \(errorCode, privacy: .public)")
    }

    private static let model: String = {
        var system = utsname()
        uname(&system)
        return withUnsafeBytes(of: &system.machine) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
    }()
}

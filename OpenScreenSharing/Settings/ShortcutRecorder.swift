import SwiftUI
#if os(iOS)
import UIKit
#else
import AppKit
#endif

enum ShortcutPress {
    case cancel
    case clear
    case shortcut(KeyShortcut)
    #if os(macOS)
    case modifierChord(NSEvent.ModifierFlags)
    #endif
}

struct ShortcutRecorder<Target: Hashable>: ViewModifier {
    @Binding var recording: Target?
    var modifierChord: Target? = nil
    let onPress: (Target, ShortcutPress) -> Bool

    private let shortcuts = KeyShortcuts.shared
    #if os(macOS)
    @State private var monitor: Any?
    #endif

    func body(content: Content) -> some View {
        content
            #if os(iOS)
            .background(KeyCapture(isActive: recording != nil) { press in recording.map { onPress($0, press) } ?? false })
            #endif
            .onChangeCompat(of: recording) { recording in
                shortcuts.isRecording = recording != nil
                #if os(macOS)
                setMonitoring(recording != nil)
                #endif
            }
            .onDisappear {
                recording = nil
                shortcuts.isRecording = false
                #if os(macOS)
                setMonitoring(false)
                #endif
            }
            #if os(macOS)
            .onReceive(NotificationCenter.default.publisher(for: NSWindow.didResignKeyNotification)) { _ in
                recording = nil
            }
            #endif
    }

    #if os(macOS)
    private func setMonitoring(_ monitoring: Bool) {
        if let monitor, !monitoring {
            NSEvent.removeMonitor(monitor)
            self.monitor = nil
            SymbolicHotKeys.resume()
        } else if monitor == nil, monitoring {
            SymbolicHotKeys.suspend()
            var held: NSEvent.ModifierFlags = []
            var spoiled = false
            monitor = NSEvent.addLocalMonitorForEvents(matching: [.keyDown, .flagsChanged]) { event in
                let flags = event.modifierFlags.intersection([.command, .option, .control, .shift])
                guard let target = recording else { return event }
                if event.type == .flagsChanged {
                    let chord = held
                    held = flags
                    if chord.isEmpty { spoiled = false }
                    guard target == modifierChord, flags.isStrictSubset(of: chord), !spoiled else { return event }
                    spoiled = true
                    if chord.rawValue.nonzeroBitCount < 2 {
                        NSSound.beep()
                    } else {
                        _ = onPress(target, .modifierChord(chord))
                    }
                    return event
                }
                spoiled = true
                let press: ShortcutPress? =
                    switch event.keyCode {
                    case 53 where flags.isEmpty: .cancel
                    case 51 where flags.isEmpty, 117 where flags.isEmpty: .clear
                    default: KeyShortcut(event: event).map(ShortcutPress.shortcut)
                    }
                if let press, onPress(target, press) { return nil }
                NSSound.beep()
                return nil
            }
        }
    }
    #endif
}

#if os(iOS)
private struct KeyCapture: UIViewRepresentable {
    let isActive: Bool
    let onKey: (ShortcutPress) -> Bool

    func makeUIView(context: Context) -> CaptureView {
        CaptureView()
    }

    func updateUIView(_ view: CaptureView, context: Context) {
        view.onKey = onKey
        if isActive, !view.isFirstResponder {
            view.becomeFirstResponder()
        } else if !isActive, view.isFirstResponder {
            view.resignFirstResponder()
        }
    }

    final class CaptureView: UIView {
        var onKey: (ShortcutPress) -> Bool = { _ in false }

        override var canBecomeFirstResponder: Bool { true }

        override var canBecomeFocused: Bool { true }

        override var keyCommands: [UIKeyCommand]? { KeyShortcuts.shared.passthroughKeyCommands(includingAssigned: true) }

        override func pressesBegan(_ presses: Set<UIPress>, with event: UIPressesEvent?) {
            let unhandled = presses.filter { press in
                guard let key = press.key else { return true }
                let flags = key.modifierFlags.intersection([.command, .alternate, .control, .shift])
                switch key.keyCode {
                case .keyboardEscape where flags.isEmpty: return !onKey(.cancel)
                case .keyboardDeleteOrBackspace where flags.isEmpty, .keyboardDeleteForward where flags.isEmpty: return !onKey(.clear)
                default: return !(KeyShortcut(key: key).map { onKey(.shortcut($0)) } ?? false)
                }
            }
            if !unhandled.isEmpty {
                super.pressesBegan(unhandled, with: event)
            }
        }
    }
}
#endif

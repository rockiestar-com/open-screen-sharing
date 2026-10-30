#if os(macOS)
import CoreGraphics

@_silgen_name("CGSMainConnectionID")
private func CGSMainConnectionID() -> Int32

@_silgen_name("CGSSetGlobalHotKeyOperatingMode")
private func CGSSetGlobalHotKeyOperatingMode(_ connection: Int32, _ mode: UInt32) -> Int32

enum SymbolicHotKeys {
    // PushSymbolicHotKeyMode forwards its mode only for processes trusted for Accessibility (HIToolbox 1272,
    // macOS 27.0.1 26A434, UUID 5D739A34-36B7-39B9-BA3B-7608B59C07A8, SyncHotKeyMode 0x18dd77470..0x18dd774bc).
    // WindowServer accepts the mode from any connection and ORs every connection's mode into the global one, counting a
    // mode with bit 31 only while its process is frontmost and dropping it with the connection; bit 1 disables every
    // symbolic hot key except the Universal Access ones (SkyLight 600, macOS 27.0.1 26A434,
    // UUID 8A3B348E-4637-3685-92D0-6CBC2F36A234, CGXUpdateGlobalHotKeyOperatingMode 0x187243a54..0x187243cd8).
    private static let disabledWhileFrontmost: UInt32 = 0x8000_0002

    private static var suspensions = 0

    static func suspend() {
        suspensions += 1
        if suspensions == 1 {
            _ = CGSSetGlobalHotKeyOperatingMode(CGSMainConnectionID(), disabledWhileFrontmost)
        }
    }

    static func resume() {
        suspensions -= 1
        if suspensions == 0 {
            _ = CGSSetGlobalHotKeyOperatingMode(CGSMainConnectionID(), 0)
        }
    }
}
#endif

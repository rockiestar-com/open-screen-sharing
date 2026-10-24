import Foundation
import Libsharing

nonisolated struct DeviceServerStateChange: Sendable {
    var state: Int32
    var errorCode: Int32
    var message: String
}

nonisolated final class DeviceServerEvents: NSObject, LibsharingDeviceServerHandlerProtocol, @unchecked Sendable {
    private let deliver: @Sendable (DeviceServerStateChange) -> Void
    private let deliverViewer: @Sendable (_ viewerID: Int64, _ connected: Bool) -> Void

    init(
        state deliver: @escaping @Sendable (DeviceServerStateChange) -> Void,
        viewer deliverViewer: @escaping @Sendable (_ viewerID: Int64, _ connected: Bool) -> Void
    ) {
        self.deliver = deliver
        self.deliverViewer = deliverViewer
    }

    func onStateChanged(_ state: Int32, errorCode: Int32, message: String?) {
        deliver(DeviceServerStateChange(state: state, errorCode: errorCode, message: message ?? ""))
    }

    func onViewerConnected(_ viewerID: Int64) {
        deliverViewer(viewerID, true)
    }

    func onViewerDisconnected(_ viewerID: Int64) {
        deliverViewer(viewerID, false)
    }

    func backlightState() -> Int32 {
        LocalBrightness.backlightState
    }
}

nonisolated final class DiskImageImportEvents: NSObject, LibsharingDiskImageImportHandlerProtocol, @unchecked Sendable {
    private let deliver: @Sendable (DiskImageImportProgress) -> Void

    init(_ deliver: @escaping @Sendable (DiskImageImportProgress) -> Void) {
        self.deliver = deliver
    }

    func onProgress(_ received: Int64, total: Int64) {
        deliver(DiskImageImportProgress(received: received, total: total > 0 ? total : nil))
    }
}

nonisolated enum PairingEvent: Sendable {
    case pin(String)
    case paired(Data)
    case failed(errorCode: Int32, message: String)
}

nonisolated final class PairingEvents: NSObject, LibsharingPairingHandlerProtocol, @unchecked Sendable {
    private let deliver: @Sendable (PairingEvent) -> Void

    init(_ deliver: @escaping @Sendable (PairingEvent) -> Void) {
        self.deliver = deliver
    }

    func onPIN(_ pin: String?) {
        deliver(.pin(pin ?? ""))
    }

    func onPaired(_ record: Data?) {
        deliver(.paired(record ?? Data()))
    }

    func onPairingFailed(_ errorCode: Int32, message: String?) {
        deliver(.failed(errorCode: errorCode, message: message ?? ""))
    }
}

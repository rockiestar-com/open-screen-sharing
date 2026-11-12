import Foundation
import GRDB

nonisolated struct Computer: Codable, Identifiable, Hashable, FetchableRecord, PersistableRecord {
    static let databaseTableName = "computers"

    var id = UUID()
    var name = ""
    var host = ""
    var port = ComputerAddress.defaultPort
    var username = ""
    var password = ""
    var authenticates = false
    var model = ""
    var enclosureColor = ""
    var lastConnected: Date?
    var thumbnail: Data?
    var createdAt = Date()
    var usesHighPerformance = false
    var networkQuality = NetworkQuality.localNetwork
    var internetMaximumFrameRate = MaximumFrameRate.sixty
    var remoteProtocol = RemoteProtocol.vnc
    var trustedFingerprint = ""
    var keepsAlive = false
    var disablesUDP = false
    var lanAddress = ""
    var prefersLANAddress = false

    var quality = SessionQuality.adaptive
    var observes = false
    var sharesClipboard = true
    var resolutionScale = VirtualDisplayMode.defaultResolutionScale
    var displayID: Int64 = -1
    var usesHDR = false
    var receivesAudio = true
    var sendsAudio = false
    var keyboardSources: [String] = []
    var windowWidth: Double = 0
    var windowHeight: Double = 0
    var locksPointer = false
    var usesNativeTouch = true

    init(name: String, address: ComputerAddress, remoteProtocol: RemoteProtocol = .vnc, username: String = "") {
        self.name = name
        host = address.host
        port = address.port
        self.remoteProtocol = remoteProtocol
        self.username = username
    }

    var deviceType: DeviceType {
        DeviceType(model: model) ?? .mac
    }

    var networkConfiguration: NetworkConfiguration {
        get { NetworkConfiguration(quality: networkQuality, internetMaximumFrameRate: internetMaximumFrameRate) }
        set {
            networkQuality = newValue.quality
            internetMaximumFrameRate = newValue.internetMaximumFrameRate
        }
    }

    var displaySelection: DisplaySelection? {
        get {
            switch displayID {
            case 0: .all
            case 1...: .display(displayID)
            default: nil
            }
        }
        set {
            switch newValue {
            case .all: displayID = 0
            case .display(let id): displayID = id
            case nil: displayID = -1
            }
        }
    }

    var address: ComputerAddress {
        get { ComputerAddress(host: host, port: port) }
        set {
            host = newValue.host
            port = newValue.port
        }
    }

    /// The LAN address dialed instead of `host` when the user forces it or when `host` can't be found or reached.
    var lanFallback: ComputerAddress? {
        lanAddress.isEmpty || ComputerAddress(host: lanAddress, port: port).matches(address) ? nil : ComputerAddress(host: lanAddress, port: port)
    }

    var addressDescription: String {
        address.description(defaultPort: remoteProtocol.defaultPort)
    }

    var displayName: String {
        name.isEmpty ? addressDescription : name
    }

    var device: DeviceAppearance {
        DeviceAppearance(model: model, enclosureColor: enclosureColor)
    }

    var memory: SessionMemory {
        SessionMemory(
            quality: quality,
            observes: observes,
            sharesClipboard: sharesClipboard,
            resolutionScale: resolutionScale,
            display: displaySelection,
            usesHDR: usesHDR,
            receivesAudio: receivesAudio,
            sendsAudio: sendsAudio,
            windowSize: windowWidth > 0 && windowHeight > 0 ? CGSize(width: windowWidth, height: windowHeight) : nil,
            locksPointer: locksPointer,
            usesNativeTouch: usesNativeTouch
        )
    }

    mutating func remember(_ memory: SessionMemory) {
        quality = memory.quality
        observes = memory.observes
        sharesClipboard = memory.sharesClipboard
        resolutionScale = memory.resolutionScale
        displaySelection = memory.display
        usesHDR = memory.usesHDR
        receivesAudio = memory.receivesAudio
        sendsAudio = memory.sendsAudio
        windowWidth = memory.windowSize.map { Double($0.width) } ?? 0
        windowHeight = memory.windowSize.map { Double($0.height) } ?? 0
        locksPointer = memory.locksPointer
        usesNativeTouch = memory.usesNativeTouch
    }

    mutating func apply(_ device: DeviceInfo) {
        model = device.model
        if !device.enclosureColor.isEmpty {
            enclosureColor = device.enclosureColor
        }
    }
}

nonisolated enum RemoteProtocol: String, Codable, Sendable {
    case vnc
    case rdp

    var defaultPort: Int {
        switch self {
        case .vnc: 5900
        case .rdp: 3389
        }
    }
}

nonisolated enum DeviceType: String, Codable, Sendable {
    case mac
    case iPhone
    case iPad

    init?(model: String) {
        let model = model.trimmingCharacters(in: .whitespaces)
        if model.hasPrefix("iPhone") {
            self = .iPhone
        } else if model.hasPrefix("iPad") {
            self = .iPad
        } else if model.hasPrefix("Mac") || model.hasPrefix("iMac") {
            self = .mac
        } else {
            return nil
        }
    }

    var isIOS: Bool { self != .mac }
}

nonisolated enum SessionQuality: String, Codable, CaseIterable, Identifiable, Sendable {
    case adaptive
    case full

    var id: Self { self }
}

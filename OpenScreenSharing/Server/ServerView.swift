import Perception
import SwiftUI
import UIKit

struct ServerView: View {
    @State private var portText: String?
    @State private var isShowingPairing = false
    @State private var isShowingDiskImage = false
    @FocusState private var focusedField: Field?

    enum Field {
        case port
        case username
        case password
        case devicePasscode
    }

    private let controller = ServerController.shared
    @Perception.Bindable private var settings = ServerSettings.shared
    private let pairing = PairingStore.shared
    private let diskImage = DiskImageStore.shared

    var body: some View {
        WithPerceptionTracking {
            Form {
                if pairing.isPaired {
                    sharingSection
                    listenSection
                    authenticationSection
                    autoUnlockSection
                    backgroundSection
                }
                setupSection
            }
            .navigationTitle("Server")
            .alert(problemAlert)
            .sheet(isPresented: $isShowingPairing) {
                PairingSetupView()
            }
            .sheet(isPresented: $isShowingDiskImage) {
                DiskImageImportView()
            }
            .onAppear {
                controller.remotePairing.start()
                controller.startProbing()
            }
            .onDisappear {
                controller.remotePairing.stop()
                controller.stopProbing()
            }
            .onChangeCompat(of: focusedField) {
                if focusedField != .port {
                    portText = nil
                }
            }
            .onChangeCompat(of: pairing.revision) {
                controller.remotePairing.refresh()
                if case .failed = controller.phase {
                    controller.stop()
                }
            }
        }
    }

    private var problemAlert: Binding<AlertState?> {
        Binding {
            if case .failed(let problem) = controller.phase {
                if problem.kind == .locationDenied {
                    return AlertState(
                        title: problem.title,
                        actions: [
                            AlertState.Action(label: String(localized: "Open Settings"), role: nil) {
                                if let url = URL(string: UIApplication.openSettingsURLString) {
                                    UIApplication.shared.open(url)
                                }
                            },
                            AlertState.Action(label: String(localized: "OK"), role: .cancel) {},
                        ])
                }
                return AlertState(problem.title, errorMessage: problem.message)
            }
            return nil
        } set: { state in
            guard state == nil else { return }
            if case .failed = controller.phase {
                controller.stop()
            }
        }
    }

    private var sharingBinding: Binding<Bool> {
        Binding(
            get: { controller.isActive },
            set: { on in
                focusedField = nil
                if on {
                    controller.start()
                } else {
                    controller.stop()
                }
            })
    }

    private var sharingSection: some View {
        Section {
            Toggle("Enabled", isOn: sharingBinding)
                .disabled(!controller.isActive && !controller.canStart)
        } footer: {
            VStack(alignment: .leading, spacing: 4) {
                if controller.tunnelThroughLocalNetwork {
                    Text("Loopback through the local network performs poorly.")
                        .foregroundStyle(.yellow)
                }
                if !controller.isActive, controller.tunnelFound == false {
                    Text("No loopback address found. Turn on [LocalDevVPN](https://apps.apple.com/app/id6755608044), or [sing-box](https://sing-box.sagernet.org) with `loopback_address` configured.")
                }
                if controller.localNetworkDenied {
                    Text("Screen Sharing isn’t allowed to use the local network. Allow it in Settings › Privacy & Security › Local Network.")
                }
            }
            .foregroundStyle(.red)
        }
    }

    private var listenSection: some View {
        Section("Listen") {
            LabeledContentCompat("Port") {
                TextField("Port", text: port, prompt: Text(verbatim: String(ServerSettings.defaultPort)))
                    .keyboardType(.numberPad)
                    .multilineTextAlignment(.trailing)
                    .foregroundStyle(isPortValid ? Color.primary : Color.red)
                    .focused($focusedField, equals: .port)
            }
        }
        .disabled(controller.isActive)
    }

    private var port: Binding<String> {
        Binding(
            get: { portText ?? String(settings.port) },
            set: { text in
                let digits = String(text.filter { $0.isASCII && $0.isNumber }.prefix(5))
                portText = digits
                if let port = Self.parsedPort(digits) {
                    settings.port = port
                }
            })
    }

    private var isPortValid: Bool {
        portText.map { Self.parsedPort($0) != nil } ?? true
    }

    private static func parsedPort(_ digits: String) -> Int? {
        guard !digits.isEmpty else { return ServerSettings.defaultPort }
        guard let port = Int(digits), ServerSettings.portRange.contains(port) else { return nil }
        return port
    }

    private var authenticationSection: some View {
        Section {
            Toggle("Enabled", isOn: $settings.authenticates)
            if settings.authenticates {
                LabeledContentCompat("Username") {
                    TextField("Username", text: $settings.username, prompt: Text("Required"))
                        .textContentType(.oneTimeCode)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .multilineTextAlignment(.trailing)
                        .focused($focusedField, equals: .username)
                        .submitLabel(.next)
                        .onSubmit { focusedField = .password }
                }
                LabeledContentCompat("Password") {
                    TextField("Password", text: $settings.password.masked { focusedField == .password }, prompt: Text("Required"))
                        .textContentType(.oneTimeCode)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .multilineTextAlignment(.trailing)
                        .focused($focusedField, equals: .password)
                        .submitLabel(.done)
                }
            }
        } header: {
            Text("Authentication")
        }
        .disabled(controller.isActive)
    }

    private var backgroundSection: some View {
        Section {
            Toggle("Enabled", isOn: $settings.runsInBackground)
            if settings.runsInBackground {
                Picker("Keep Running With", selection: $settings.backgroundMode) {
                    Text("Location Service").tag(ServerBackgroundMode.location)
                    Text("Audio Service").tag(ServerBackgroundMode.audio)
                }
                .pickerStyle(.menu)
            }
        } header: {
            Text("Background Service")
        } footer: {
            Text("Screen Sharing needs to stay running in the background to keep sharing this device’s screen.")
        }
        .onChangeCompat(of: settings.runsInBackground) { controller.updateBackground() }
        .onChangeCompat(of: settings.backgroundMode) { controller.updateBackground() }
    }

    private var autoUnlockSection: some View {
        Section {
            Toggle("Enabled", isOn: $settings.autoUnlocks)
            if settings.autoUnlocks {
                Picker("Passcode Type", selection: $settings.autoUnlockPasscodeType) {
                    Text("4-Digit or 6-Digit Code").tag(ServerPasscodeType.pin)
                    Text("Custom Numeric Code").tag(ServerPasscodeType.numeric)
                    Text("Custom Alphanumeric Code").tag(ServerPasscodeType.alphanumeric)
                }
                .pickerStyle(.menu)
                LabeledContentCompat("Device Passcode") {
                    TextField("Device Passcode", text: $settings.autoUnlockPasscode.masked { focusedField == .devicePasscode }, prompt: Text("Required"))
                        .textContentType(.oneTimeCode)
                        .keyboardType(settings.autoUnlockPasscodeType == .alphanumeric ? .asciiCapable : .asciiCapableNumberPad)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .multilineTextAlignment(.trailing)
                        .focused($focusedField, equals: .devicePasscode)
                        .submitLabel(.done)
                }
            }
        } header: {
            Text("Automatic Unlock")
        } footer: {
            VStack(alignment: .leading, spacing: 4) {
                Text("Screen Sharing needs your passcode to automatically unlock the device when connected.")
                if let problem = settings.autoUnlockProblem {
                    Text(problem)
                        .foregroundStyle(.red)
                }
            }
        }
        .disabled(controller.isActive)
    }

    private var setupSection: some View {
        Section {
            Button(pairing.isPaired ? "Pair Again" : "Pair") { isShowingPairing = true }
            Button(diskImage.isImported ? "Import Disk Image Again" : "Import Disk Image") { isShowingDiskImage = true }
        } header: {
            Text("Setup")
        } footer: {
            Text("Before sharing this device’s screen, pair and import the disk image.")
        }
        .disabled(controller.isActive)
    }
}

import Libsharing
import Perception
import SwiftUI
import UniformTypeIdentifiers

struct MacDiskImageImportView: View {
    let run: (@escaping () async throws -> Void) -> Void

    @State private var host = ""
    @State private var port = ""
    @State private var username = ""
    @State private var authentication = Authentication.password
    @State private var password = ""
    @State private var privateKey: PrivateKey?
    @State private var passphrase = ""
    @State private var isChoosingKey = false
    @State private var alert: AlertState?
    @FocusState private var focusedField: Field?

    enum Field {
        case host
        case port
        case username
        case password
        case passphrase
    }

    enum Authentication {
        case password
        case publicKey
    }

    struct PrivateKey {
        var name: String
        var data: Data
        var isEncrypted: Bool
    }

    private static let defaultPort = 22

    private let store = DiskImageStore.shared

    var body: some View {
        WithPerceptionTracking {
            SheetForm(Text("Import from Mac")) {
                Section {
                    LabeledContentCompat("Host") {
                        TextField("Host", text: $host, prompt: Text("Required"))
                            .multilineTextAlignment(.trailing)
                            .keyboardType(.URL)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .focused($focusedField, equals: .host)
                            .submitLabel(.next)
                            .onSubmit { focusedField = .username }
                    }
                    LabeledContentCompat("Port") {
                        TextField("Port", text: portDigits, prompt: Text(verbatim: String(Self.defaultPort)))
                            .multilineTextAlignment(.trailing)
                            .keyboardType(.numberPad)
                            .foregroundStyle(parsedPort == nil ? Color.red : Color.primary)
                            .focused($focusedField, equals: .port)
                    }
                    LabeledContentCompat("User Name") {
                        TextField("User Name", text: $username, prompt: Text("Required"))
                            .multilineTextAlignment(.trailing)
                            .textContentType(.oneTimeCode)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .focused($focusedField, equals: .username)
                            .submitLabel(authentication == .password ? .next : .done)
                            .onSubmit { focusedField = authentication == .password ? .password : nil }
                    }
                    Picker("Authentication", selection: $authentication) {
                        Text("Password").tag(Authentication.password)
                        Text("Public Key").tag(Authentication.publicKey)
                    }
                    .pickerStyle(.menu)
                    switch authentication {
                    case .password:
                        LabeledContentCompat("Password") {
                            secretField("Password", text: $password, field: .password)
                        }
                    case .publicKey:
                        Button {
                            isChoosingKey = true
                        } label: {
                            LabeledContentCompat("Private Key") {
                                HStack {
                                    if let privateKey {
                                        Text(verbatim: privateKey.name)
                                            .lineLimit(1)
                                            .truncationMode(.middle)
                                    }
                                    Text("Choose…")
                                        .foregroundStyle(.tint)
                                }
                            }
                        }
                        .foregroundStyle(.primary)
                        if privateKey?.isEncrypted == true {
                            LabeledContentCompat("Passphrase") {
                                secretField("Passphrase", text: $passphrase, field: .passphrase)
                            }
                        }
                    }
                } footer: {
                    if store.isImporting {
                        DiskImageImportProgressView(waiting: "Connecting…")
                    }
                }
                .disabled(store.isImporting)
            }
            .navigationBarBackButtonHidden(store.isImporting)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    if store.isImporting {
                        Button("Cancel", role: .cancel) { store.cancel() }
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Import", role: .confirmCompat, action: start)
                        .disabled(!isValid || store.isImporting)
                }
            }
            .fileImporter(isPresented: $isChoosingKey, allowedContentTypes: [.item]) { result in
                if case .success(let url) = result {
                    choosePrivateKey(url)
                }
            }
            .alert($alert)
        }
    }

    private func secretField(_ title: LocalizedStringKey, text: Binding<String>, field: Field) -> some View {
        TextField(title, text: text.masked { focusedField == field }, prompt: Text("Required"))
            .multilineTextAlignment(.trailing)
            .textContentType(.oneTimeCode)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
            .focused($focusedField, equals: field)
            .submitLabel(.done)
            .onSubmit {
                if isValid {
                    start()
                }
            }
    }

    private var portDigits: Binding<String> {
        Binding {
            port
        } set: {
            port = String($0.filter { $0.isASCII && $0.isNumber }.prefix(5))
        }
    }

    private var parsedPort: Int? {
        guard !port.isEmpty else { return Self.defaultPort }
        guard let value = Int(port), (1...65535).contains(value) else { return nil }
        return value
    }

    private var trimmedHost: String {
        host.trimmingCharacters(in: .whitespaces)
    }

    private var trimmedUsername: String {
        username.trimmingCharacters(in: .whitespaces)
    }

    private var isValid: Bool {
        guard !trimmedHost.isEmpty, !trimmedUsername.isEmpty, parsedPort != nil else { return false }
        switch authentication {
        case .password:
            return !password.isEmpty
        case .publicKey:
            guard let privateKey else { return false }
            return !privateKey.isEncrypted || !passphrase.isEmpty
        }
    }

    private func choosePrivateKey(_ url: URL) {
        Task {
            do {
                let data = try await url.pickedFileContents()
                privateKey = PrivateKey(name: url.lastPathComponent, data: data, isEncrypted: LibsharingPrivateKeyNeedsPassphrase(data))
                passphrase = ""
            } catch {
                alert = AlertState(String(localized: "Couldn’t Read the Private Key"), error: error)
            }
        }
    }

    private func start() {
        guard isValid, let port = parsedPort else { return }
        focusedField = nil
        let usesKey = authentication == .publicKey
        let request = MacImportRequest(
            host: trimmedHost,
            port: port,
            username: trimmedUsername,
            password: usesKey ? "" : password,
            privateKey: usesKey ? privateKey?.data : nil,
            passphrase: usesKey ? passphrase : ""
        )
        run { try await store.importFromMac(request) }
    }
}

import Perception
import SwiftUI
import UIKit
import UniformTypeIdentifiers

struct PairingSetupView: View {
    @Environment(\.dismiss) private var dismiss
    @State private var isPickingFile = false
    @State private var alert: AlertState?

    private let pairing = PairingStore.shared
    private let session = ServerController.shared.pairingSession

    var body: some View {
        WithPerceptionTracking {
            NavigationStackCompat {
                SheetForm(Text("Pair with This Device")) {
                    Section {
                        if case .code(let pin) = session.state {
                            PairingCodeView(pin: pin)
                        } else {
                            Button(action: pairInSettings) {
                                HStack {
                                    Text("Pair in Settings")
                                    Spacer()
                                    if session.isInProgress {
                                        ProgressView()
                                    }
                                }
                            }
                            .disabled(session.isInProgress)
                        }
                    } footer: {
                        Text("1. Go to Settings › Privacy & Security › Developer Mode.\n2. Tap “Pair with \(session.hostName)”.\n3. Enter the pairing code shown by Screen Sharing.")
                    }
                    Section {
                        Button("Import Pairing File") { isPickingFile = true }
                    }
                    .disabled(session.isInProgress)
                }
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel", role: .cancel) {
                            session.cancel()
                            dismiss()
                        }
                    }
                }
                .fileImporter(isPresented: $isPickingFile, allowedContentTypes: [.item]) { result in
                    if case .success(let url) = result {
                        importFile(url)
                    }
                }
                .alert(problemAlert)
            }
            .interactiveDismissDisabled(session.isInProgress)
            .onChangeCompat(of: pairing.revision) { dismiss() }
        }
    }

    private var problemAlert: Binding<AlertState?> {
        Binding {
            if let alert {
                return alert
            }
            if case .failed(let problem) = session.state {
                return AlertState(problem.title, errorMessage: problem.message)
            }
            return nil
        } set: { state in
            guard state == nil else { return }
            alert = nil
            session.acknowledgeFailure()
        }
    }

    private func pairInSettings() {
        session.start()
        guard session.isInProgress else { return }
        Task {
            if let url = URL(string: "App-Prefs:"), await UIApplication.shared.open(url) { return }
            if let url = URL(string: UIApplication.openSettingsURLString) {
                _ = await UIApplication.shared.open(url)
            }
        }
    }

    private func importFile(_ url: URL) {
        Task {
            do {
                try pairing.save(try await url.pickedFileContents())
            } catch {
                alert = AlertState(String(localized: "Couldn’t Import the Pairing File"), error: error)
            }
        }
    }
}

private struct PairingCodeView: View {
    let pin: String

    var body: some View {
        Text(verbatim: PairingSession.grouped(pin))
            .font(.system(size: 48, weight: .semibold, design: .monospaced))
            .minimumScaleFactor(0.5)
            .lineLimit(1)
            .textSelection(.enabled)
            .accessibilityLabel(Text(verbatim: pin.map(String.init).joined(separator: " ")))
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
    }
}

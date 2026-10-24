import Perception
import SwiftUI
import UniformTypeIdentifiers

struct DiskImageImportView: View {
    @Environment(\.dismiss) private var dismiss
    @State private var isPickingFile = false
    @State private var alert: AlertState?

    private let store = DiskImageStore.shared

    var body: some View {
        WithPerceptionTracking {
            NavigationStackCompat {
                SheetForm(Text("Import Disk Image")) {
                    Section {
                        Button("From Files") { isPickingFile = true }
                        NavigationLink("From a Mac") {
                            MacDiskImageImportView(run: run)
                        }
                        NavigationLink("From URL") {
                            DiskImageDownloadView(run: run)
                        }
                    } footer: {
                        if store.isImporting {
                            DiskImageImportProgressView()
                        }
                    }
                    .disabled(store.isImporting)
                }
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel", role: .cancel) {
                            store.cancel()
                            dismiss()
                        }
                    }
                }
                .fileImporter(isPresented: $isPickingFile, allowedContentTypes: [.folder, .zip]) { result in
                    if case .success(let url) = result {
                        run { try await store.importFile(at: url) }
                    }
                }
            }
            .interactiveDismissDisabled(store.isImporting)
            .alert($alert)
        }
    }

    private func run(_ operation: @escaping () async throws -> Void) {
        guard !store.isImporting else { return }
        Task {
            do {
                try await operation()
                dismiss()
            } catch is CancellationError {
            } catch {
                alert = AlertState(String(localized: "Couldn’t Import the Disk Image"), error: error)
            }
        }
    }
}

struct DiskImageImportProgressView: View {
    var waiting: LocalizedStringKey = "Importing…"

    private let store = DiskImageStore.shared

    var body: some View {
        WithPerceptionTracking {
            if let progress = store.progress, progress.received > 0 {
                InlineProgress(verbatim: progress.localizedDescription)
            } else {
                InlineProgress(waiting)
            }
        }
    }
}

struct DiskImageDownloadView: View {
    let run: (@escaping () async throws -> Void) -> Void

    @State private var url = DiskImageStore.defaultURL
    @FocusState private var isFocused: Bool

    private let store = DiskImageStore.shared

    private var trimmedURL: String {
        url.trimmingCharacters(in: .whitespaces)
    }

    var body: some View {
        WithPerceptionTracking {
            SheetForm(Text("Download Disk Image")) {
                Section {
                    Group {
                        if #available(iOS 16, *) {
                            TextField("URL", text: singleLine, prompt: Text("Required"), axis: .vertical)
                        } else {
                            TextField("URL", text: singleLine, prompt: Text("Required"))
                                .onSubmit(start)
                        }
                    }
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .focused($isFocused)
                } header: {
                    Text("URL")
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
                    Button("Download", role: .confirmCompat, action: start)
                        .disabled(trimmedURL.isEmpty || store.isImporting)
                }
            }
        }
    }

    private var singleLine: Binding<String> {
        Binding {
            url
        } set: { text in
            url = text.filter { !$0.isNewline }
            if text.contains(where: \.isNewline) {
                start()
            }
        }
    }

    private func start() {
        guard !trimmedURL.isEmpty else { return }
        isFocused = false
        let url = trimmedURL
        run { try await store.download(from: url) }
    }
}

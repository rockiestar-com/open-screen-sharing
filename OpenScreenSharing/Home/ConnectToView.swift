import Perception
import SwiftUI

struct ConnectToView: View {
    let link: RemoteLink?
    let close: () -> Void
    let signIn: (NewComputer) -> Void
    let signedIn: (SignedInComputer) -> Void

    @Environment(NearbyBrowser.self) private var nearby
    private let store = ComputerStore.shared
    @State private var text = ""
    @State private var problem: SetupProblem?
    @State private var task: Task<Void, Never>?
    @State private var checkedID: NearbyComputer.ID?
    @State private var nearbyProblem: SetupProblem?
    @State private var details: NearbyComputer?
    @State private var opensLink = true
    @FocusState private var isFocused: Bool

    private var isChecking: Bool { task != nil }

    private var typed: String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private var found: [NearbyComputer] {
        nearby.computers.filter { found in
            #if os(iOS) && (DEBUG || SIDELOAD)
            guard found.name != ServerController.shared.advertisedName else { return false }
            #endif
            return !store.computers.contains { saved in
                saved.remoteProtocol == found.remoteProtocol
                    && (found.address.map(saved.address.matches) == true
                        || nearby.computer(matching: saved.address, remoteProtocol: saved.remoteProtocol)?.id == found.id)
            }
        }
    }

    var body: some View {
        WithPerceptionTracking {
            SheetForm(Text("New Connection")) {
                Section {
                    TextField("Server Address", text: $text, prompt: Text("Hostname"))
                        #if os(iOS)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        #endif
                        .autocorrectionDisabled()
                        .focused($isFocused)
                        .submitLabel(.next)
                        .onSubmit(start)
                        .disabled(isChecking)
                        .onChangeCompat(of: text) { problem = nil }
                } footer: {
                    if isChecking, checkedID == nil {
                        InlineProgress("Connecting…")
                    } else if let problem {
                        SetupProblemView(problem: problem)
                    }
                }
                Section {
                    if found.isEmpty {
                        placeholder
                    }
                    ForEach(found) { computer in
                        Button {
                            choose(computer)
                        } label: {
                            HStack {
                                ComputerRow(
                                    name: computer.name,
                                    detail: computer.address?.description(defaultPort: computer.remoteProtocol.defaultPort),
                                    device: DeviceAppearance(model: computer.model ?? ""), availability: .available)
                                if checkedID == computer.id {
                                    ProgressView()
                                        .controlSize(.small)
                                }
                            }
                        }
                        #if os(macOS)
                        .buttonStyle(.plain)
                        #endif
                        .foregroundStyle(.primary)
                        .disabled(computer.address == nil || isChecking)
                        .contextMenu {
                            Button {
                                details = computer
                            } label: { Label("View Details", systemImage: "info.circle") }
                        }
                    }
                } header: {
                    Text("Network")
                } footer: {
                    if let nearbyProblem {
                        SetupProblemView(problem: nearbyProblem)
                    }
                }
            }
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel", role: .cancel) {
                        task?.cancel()
                        close()
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Next", action: start)
                        .disabled(typed.isEmpty || isChecking)
                }
            }
            .sheet(item: $details) { computer in
                NearbyDetailsView(computer: computer)
            }
            .interactiveDismissDisabled(isChecking)
            .onAppear {
                guard opensLink, let link else { return }
                opensLink = false
                text = (link.username.map { "\($0)@" } ?? "") + link.address.description(defaultPort: link.remoteProtocol.defaultPort)
                connect(
                    to: ConnectionTarget(
                        host: link.address.host, port: link.address.port, remoteProtocol: link.remoteProtocol, username: link.username,
                        password: link.password))
            }
            .onDisappear {
                task?.cancel()
                task = nil
            }
        }
    }

    @ViewBuilder
    private var placeholder: some View {
        if case .failed(let message) = nearby.state {
            VStack(alignment: .leading, spacing: 4) {
                Text("Can’t Look for Nearby Computers")
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        } else if nearby.hasSearched {
            Text("No Computers Found")
                .foregroundStyle(.secondary)
        } else {
            InlineProgress("Looking for Computers…")
        }
    }

    private func choose(_ computer: NearbyComputer) {
        guard !isChecking, let address = computer.address else { return }
        nearbyProblem = nil
        checkedID = computer.id
        task = Task {
            let problem = await check(host: address.host, port: address.port, remoteProtocol: computer.remoteProtocol) { server in
                NewComputer(name: computer.name, model: computer.model, server: server)
            }
            guard !Task.isCancelled else { return }
            task = nil
            checkedID = nil
            nearbyProblem = problem
        }
    }

    private func start() {
        guard !isChecking, !typed.isEmpty else { return }
        guard let target = ConnectionTarget(parsing: typed) else {
            problem = SetupProblem(summary: String(localized: "“\(typed)” isn’t a valid address."))
            return
        }
        connect(to: target)
    }

    private func connect(to target: ConnectionTarget) {
        problem = nil
        nearbyProblem = nil
        task = Task {
            let problem = await check(host: target.host, port: target.port, remoteProtocol: target.remoteProtocol) { server in
                let found = nearby.computer(matching: server.address, remoteProtocol: server.remoteProtocol)
                return NewComputer(
                    name: found?.name ?? server.address.bonjourServiceName ?? server.address.host,
                    model: found?.model,
                    username: target.username ?? "",
                    password: target.password ?? "",
                    server: server
                )
            }
            guard !Task.isCancelled else { return }
            task = nil
            if let problem {
                self.problem = problem
                isFocused = true
            }
        }
    }

    private func check(
        host: String, port: Int?, remoteProtocol: RemoteProtocol?, making makeComputer: (ProbedServer) -> NewComputer
    ) async -> SetupProblem? {
        let server: ProbedServer
        switch await ServerProber.probe(host: host, port: port, remoteProtocol: remoteProtocol) {
        case .success(let found):
            server = found
        case .failure(let failure):
            return SetupProblem(failure, host: host, asksForUsername: true)
        }
        guard !Task.isCancelled else { return nil }
        let computer = makeComputer(server)
        guard let credentials = server.authentication.openCredentials else {
            guard server.authentication.account || server.authentication.vncPassword else {
                return SetupProblem(summary: String(localized: "“\(computer.name)” offers no sign-in method this app supports."))
            }
            signIn(computer)
            return nil
        }
        switch await ServerProber.checkLogin(server, credentials: credentials) {
        case .success(let check):
            guard !Task.isCancelled else { return nil }
            signedIn(computer.signedIn(credentials, check: check))
            return nil
        case .failure(let failure):
            return SetupProblem(failure, host: host, asksForUsername: false)
        }
    }
}

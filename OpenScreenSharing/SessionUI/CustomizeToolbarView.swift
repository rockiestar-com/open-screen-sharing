import Perception
import SwiftUI

struct CustomizeToolbarView: View {
    let controller: SessionController
    private let layout: ToolbarLayout
    let slots: Int?

    @Environment(\.dismiss) private var dismiss
    @State private var isConfirmingReset = false

    init(controller: SessionController, slots: Int?) {
        self.controller = controller
        layout = .layout(for: controller)
        self.slots = slots
    }

    var body: some View {
        WithPerceptionTracking {
            let plan = layout.plan(for: controller, slots: slots)
            let categories = SessionCommandCategory.allCases.filter { !layout.otherCommands(in: $0).isEmpty }
            NavigationStackCompat {
                SheetForm(Text("Customize Toolbar")) {
                    Section {
                        ForEach(layout.bar, id: \.shownRowID) { command in
                            WithPerceptionTracking {
                                shownRow(command, inMore: plan.overflow.contains(command))
                                    .modifier(ReorderActions(layout: layout, command: command))
                            }
                        }
                        .onMove { source, destination in layout.move(fromOffsets: source, toOffset: destination) }
                        .onDelete { offsets in layout.remove(atOffsets: offsets) }
                    }
                    ForEach(categories) { category in
                        WithPerceptionTracking {
                            Section {
                                ForEach(layout.otherCommands(in: category), id: \.otherRowID) { command in
                                    WithPerceptionTracking {
                                        Button {
                                            layout.add(command)
                                        } label: {
                                            HStack(spacing: 12) {
                                                Image(systemName: "plus.circle.fill")
                                                    .symbolRenderingMode(.palette)
                                                    .foregroundStyle(.white, .green)
                                                    .font(.title2)
                                                    .accessibilityHidden(true)
                                                row(command, inMore: false)
                                            }
                                            .contentShape(Rectangle())
                                        }
                                        .buttonStyle(.plain)
                                        .accessibilityLabel(Text("Add \(String(localized: command.title)) to Toolbar"))
                                        .accessibilityValue(notAvailableValue(command))
                                        .deleteDisabled(true)
                                        .moveDisabled(true)
                                    }
                                }
                            } header: {
                                Text(String(localized: category.title))
                            }
                        }
                    }
                    Section {
                        Button("Reset to Default") {
                            if layout.isDefault {
                                layout.reset()
                            } else {
                                isConfirmingReset = true
                            }
                        }
                        .disabled(!layout.isCustomized)
                        .confirmationDialog("Reset Toolbar?", isPresented: $isConfirmingReset, titleVisibility: .visible) {
                            Button("Reset Toolbar", role: .destructive) { layout.reset() }
                            Button("Cancel", role: .cancel) {}
                        }
                    }
                }
                #if os(iOS)
                .environment(\.editMode, .constant(.active))
                #endif
                .animation(.default, value: layout.bar)
                #if os(iOS)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        CloseButton { dismiss() }
                    }
                }
                #else
                .frame(width: 420, height: 560)
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) {
                        CloseButton { dismiss() }
                    }
                }
                #endif
            }
            .phonePresentationDetents([.medium, .large])
        }
    }

    @ViewBuilder
    private func shownRow(_ command: SessionCommand, inMore: Bool) -> some View {
        #if os(iOS)
        row(command, inMore: inMore)
        #else
        HStack(spacing: 12) {
            Button {
                layout.remove(command)
            } label: {
                Image(systemName: "minus.circle.fill")
                    .symbolRenderingMode(.palette)
                    .foregroundStyle(.white, .red)
                    .font(.title2)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text("Remove from Toolbar"))
            row(command, inMore: inMore)
            Image(systemName: "line.3.horizontal")
                .foregroundStyle(.tertiary)
                .accessibilityHidden(true)
        }
        #endif
    }

    private func row(_ command: SessionCommand, inMore: Bool) -> some View {
        let offered = command.isOffered(by: controller)
        return HStack {
            Label(String(localized: command.title), systemImage: command.systemImage)
            Spacer(minLength: 8)
            if inMore {
                Text("In More")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .foregroundStyle(offered ? .primary : .tertiary)
        .accessibilityValue(notAvailableValue(command))
    }

    private func notAvailableValue(_ command: SessionCommand) -> Text {
        command.isOffered(by: controller) ? Text(verbatim: "") : Text("Not Available")
    }
}

private extension SessionCommand {
    var shownRowID: String { "shown." + rawValue }
    var otherRowID: String { "other." + rawValue }
}

private struct ReorderActions: ViewModifier {
    let layout: ToolbarLayout
    let command: SessionCommand

    func body(content: Content) -> some View {
        WithPerceptionTracking {
            if #available(iOS 16, *) {
                content.accessibilityActions {
                    if command != layout.bar.first {
                        Button("Move Up") { layout.move(command, by: -1) }
                    }
                    if command != layout.bar.last {
                        Button("Move Down") { layout.move(command, by: 1) }
                    }
                    Button("Remove from Toolbar") { layout.remove(command) }
                }
            } else {
                content
                    .accessibilityAction(named: Text("Move Up")) { layout.move(command, by: -1) }
                    .accessibilityAction(named: Text("Move Down")) { layout.move(command, by: 1) }
                    .accessibilityAction(named: Text("Remove from Toolbar")) { layout.remove(command) }
            }
        }
    }
}


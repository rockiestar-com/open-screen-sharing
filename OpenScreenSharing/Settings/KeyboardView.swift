#if os(iOS)
import GameController
#endif
import Perception
import SwiftUI

struct KeyboardView: View {
    private let shortcuts = KeyShortcuts.shared
    #if os(iOS)
    private let keyboard = ConnectedKeyboard.shared
    #endif
    @State private var editing: Editing?

    private struct Editing: Identifiable {
        let mapping: KeyMapping
        let target: MappingTarget
        let isNew: Bool

        var id: UUID { mapping.id }
    }

    var body: some View {
        WithPerceptionTracking {
            Group {
                #if os(iOS)
                if keyboard.isConnected {
                    form
                } else {
                    ContentUnavailableViewCompat {
                        Label("No Keyboard", systemImage: "keyboard")
                    } description: {
                        Text("Connect a keyboard to set up shortcuts.")
                    }
                }
                #else
                form
                #endif
            }
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .navigationTitle("Keyboard")
            .sheet(item: $editing) { editing in
                KeyMappingEditor(mapping: editing.mapping, target: editing.target, isNew: editing.isNew)
            }
        }
    }

    private var form: some View {
        Form {
            ForEach(MappingTarget.allCases) { target in
                WithPerceptionTracking {
                    Section(target.title) {
                        ForEach(shortcuts.mappings(for: target)) { mapping in
                            HStack {
                                Button {
                                    editing = Editing(mapping: mapping, target: target, isNew: false)
                                } label: {
                                    HStack {
                                        Text(verbatim: mapping.trigger.displayName)
                                        Image(systemName: "arrow.right")
                                            .foregroundStyle(.secondary)
                                        Text(verbatim: mapping.displayName(for: target))
                                            .lineLimit(1)
                                        Spacer()
                                    }
                                    .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                                Button {
                                    shortcuts.removeMapping(mapping.id, from: target)
                                } label: {
                                    Image(systemName: "xmark.circle.fill")
                                }
                                .buttonStyle(.borderless)
                                .foregroundStyle(.secondary)
                                .accessibilityLabel(Text("Delete Mapping"))
                            }
                        }
                        .onDelete { offsets in
                            for mapping in offsets.map({ shortcuts.mappings(for: target)[$0] }) {
                                shortcuts.removeMapping(mapping.id, from: target)
                            }
                        }
                        Button {
                            editing = Editing(mapping: KeyMapping(id: UUID(), trigger: .unset, steps: []), target: target, isNew: true)
                        } label: {
                            Label("Add Mapping", systemImage: "plus")
                        }
                        .formRowButtonStyleCompat()
                    }
                }
            }
            Section("Shortcuts") {
                ForEach(ShortcutGroup.allCases) { group in
                    NavigationLink {
                        ShortcutGroupView(group: group)
                    } label: {
                        Text(verbatim: group.title)
                    }
                }
            }
        }
        #if os(macOS)
        .formStyle(.grouped)
        .unboundedFormWidth()
        #endif
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Menu {
                    Button(role: .destructive) {
                        shortcuts.restoreDefaults()
                    } label: {
                        Label("Restore Defaults", systemImage: "eraser.fill")
                    }
                    .disabled(!shortcuts.isCustomized)
                } label: {
                    Label("Others", systemImage: "ellipsis.circle")
                }
            }
        }
    }
}

private struct ShortcutGroupView: View {
    let group: ShortcutGroup

    private let shortcuts = KeyShortcuts.shared
    @State private var recording: Recording?
    @State private var alert: AlertState?

    var body: some View {
        Form {
            Section {
                ForEach(group.actions) { action in
                    WithPerceptionTracking {
                        row(.action(action), title: action.title, shortcut: shortcuts.shortcut(for: action)?.displayName) {
                            shortcuts.set(nil, for: action)
                        }
                        #if os(macOS)
                        if action == .pointerLock {
                            row(.pointerRelease, title: String(localized: "Release Pointer"), shortcut: EventModifiers(shortcuts.pointerRelease).symbols, clear: nil)
                        }
                        #endif
                    }
                }
            }
        }
        #if os(macOS)
        .formStyle(.grouped)
        .unboundedFormWidth()
        #else
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .navigationTitle(group.title)
        .alert($alert)
        #if os(macOS)
        .modifier(ShortcutRecorder(recording: $recording, modifierChord: .pointerRelease, onPress: record))
        #else
        .modifier(ShortcutRecorder(recording: $recording, onPress: record))
        #endif
    }

    private func row(_ target: Recording, title: String, shortcut: String?, clear: (() -> Void)?) -> some View {
        HStack {
            Text(verbatim: title)
            Spacer()
            if let clear, shortcut != nil, recording != target {
                Button(action: clear) {
                    Image(systemName: "xmark.circle.fill")
                }
                .buttonStyle(.borderless)
                .foregroundStyle(.secondary)
                .accessibilityLabel(Text("Clear Shortcut"))
            }
            ShortcutButton(isRecording: recording == target, title: title, shortcut: shortcut) {
                recording = recording == target ? nil : target
            }
        }
    }

    private enum Recording: Hashable {
        case action(ShortcutAction)
        #if os(macOS)
        case pointerRelease
        #endif
    }

    private func record(_ target: Recording, _ press: ShortcutPress) -> Bool {
        switch press {
        case .cancel:
            recording = nil
        case .clear:
            guard case .action(let action) = target else { return false }
            recording = nil
            shortcuts.set(nil, for: action)
        case .shortcut(let shortcut):
            guard case .action(let action) = target, shortcut.isValidTrigger, shortcut.keyboardShortcut != nil else { return false }
            recording = nil
            assign(shortcut, to: action)
        #if os(macOS)
        case .modifierChord(let flags):
            recording = nil
            shortcuts.setPointerRelease(flags)
        #endif
        }
        return true
    }

    private func assign(_ shortcut: KeyShortcut, to action: ShortcutAction) {
        if let other = shortcuts.action(using: shortcut), other != action {
            alert = AlertState(
                title: String(localized: "“\(shortcut.displayName)” Is Already in Use"),
                message: String(localized: "It’s assigned to “\(other.title)”. Replacing it removes that shortcut."),
                actions: [
                    AlertState.Action(label: String(localized: "Cancel"), role: .cancel) {},
                    AlertState.Action(label: String(localized: "Replace"), role: nil, isDefault: true) {
                        shortcuts.set(nil, for: other)
                        shortcuts.set(shortcut, for: action)
                    },
                ]
            )
            return
        }
        let conflicts = MappingTarget.allCases.compactMap { target in
            shortcuts.mapping(triggeredBy: shortcut, for: target).map { (target: target, mapping: $0) }
        }
        if !conflicts.isEmpty {
            let names = conflicts.map { $0.mapping.displayName(for: $0.target) }.formatted(.list(type: .and))
            alert = AlertState(
                title: String(localized: "“\(shortcut.displayName)” Is Already in Use"),
                message: conflicts.count == 1
                    ? String(localized: "It’s mapped to \(names). Replacing it removes that mapping.")
                    : String(localized: "It’s mapped to \(names). Replacing it removes those mappings."),
                actions: [
                    AlertState.Action(label: String(localized: "Cancel"), role: .cancel) {},
                    AlertState.Action(label: String(localized: "Replace"), role: nil, isDefault: true) {
                        for conflict in conflicts {
                            shortcuts.removeMapping(conflict.mapping.id, from: conflict.target)
                        }
                        shortcuts.set(shortcut, for: action)
                    },
                ]
            )
            return
        }
        shortcuts.set(shortcut, for: action)
    }
}

struct ShortcutButton: View {
    let isRecording: Bool
    let title: String
    let shortcut: String?
    var prompt: LocalizedStringKey = "Type Shortcut"
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            ShortcutButtonLabel(isRecording: isRecording, shortcut: shortcut, prompt: prompt)
        }
        .buttonStyle(.bordered)
        .tint(isRecording ? .accentColor : nil)
        .id(isRecording)
        .accessibilityLabel(Text(verbatim: title))
        .accessibilityValue(isRecording ? Text(prompt) : shortcut.map { Text(verbatim: $0) } ?? Text("None"))
    }
}

private struct ShortcutButtonLabel: View {
    let isRecording: Bool
    let shortcut: String?
    let prompt: LocalizedStringKey

    var body: some View {
        if isRecording {
            Text(prompt)
        } else if let shortcut {
            Text(verbatim: shortcut)
        } else {
            Text("None")
                .foregroundStyle(.secondary)
        }
    }
}

private struct KeyMappingEditor: View {
    let id: UUID
    let target: MappingTarget
    let isNew: Bool

    private let shortcuts = KeyShortcuts.shared
    @State private var trigger: KeyShortcut
    @State private var steps: [Step]
    @State private var recording: Field?
    @State private var alert: AlertState?
    @Environment(\.dismiss) private var dismiss

    init(mapping: KeyMapping, target: MappingTarget, isNew: Bool) {
        id = mapping.id
        self.target = target
        self.isNew = isNew
        _trigger = State(initialValue: mapping.trigger)
        _steps = State(initialValue: mapping.steps.map(Step.init))
    }

    private struct Step: Identifiable {
        let id = UUID()
        let sendsText: Bool
        var shortcut: KeyShortcut
        var text: String

        init(_ step: KeyMapping.Step) {
            switch step {
            case .key(let shortcut):
                sendsText = false
                self.shortcut = shortcut
                text = ""
            case .text(let text):
                sendsText = true
                shortcut = .unset
                self.text = text
            }
        }

        var value: KeyMapping.Step {
            sendsText ? .text(text) : .key(shortcut)
        }

        var isComplete: Bool {
            sendsText ? !text.isEmpty : !shortcut.isUnset && (shortcut.hasCommandModifier || shortcut.isNamed)
        }
    }

    private enum Field: Hashable {
        case trigger
        case key(UUID)
    }

    private static let appleModifiers: [(flag: EventModifiers, label: String, title: String)] = [
        (.control, "⌃", String(localized: "Control Key")),
        (.option, "⌥", String(localized: "Option Key")),
        (.shift, "⇧", String(localized: "Shift Key")),
        (.command, "⌘", String(localized: "Command Key")),
    ]

    private static let pcModifiers: [(flag: EventModifiers, label: String, title: String)] = [
        (.control, "Ctrl", String(localized: "Ctrl Key")),
        (.option, "Alt", String(localized: "Alt Key")),
        (.shift, "Shift", String(localized: "Shift Key")),
        (.command, "Win", String(localized: "Windows Key")),
    ]

    private var modifiers: [(flag: EventModifiers, label: String, title: String)] {
        target == .pc ? Self.pcModifiers : Self.appleModifiers
    }

    private var mapping: KeyMapping {
        KeyMapping(id: id, trigger: trigger, steps: steps.map(\.value))
    }

    private var isComplete: Bool {
        !trigger.isUnset && !steps.isEmpty && steps.allSatisfy(\.isComplete)
    }

    private func binding(for step: Step) -> Binding<Step> {
        Binding(
            get: { steps.first { $0.id == step.id } ?? step },
            set: { value in
                guard let index = steps.firstIndex(where: { $0.id == step.id }) else { return }
                steps[index] = value
            }
        )
    }

    var body: some View {
        NavigationStackCompat {
            SheetForm(isNew ? Text("Add Mapping") : Text("Edit Mapping")) {
                Section {
                    HStack {
                        Text("Shortcut")
                        Spacer()
                        ShortcutButton(isRecording: recording == .trigger, title: String(localized: "Shortcut"), shortcut: trigger.isUnset ? nil : trigger.displayName) {
                            recording = recording == .trigger ? nil : .trigger
                        }
                    }
                }
                Section("Sends") {
                    ForEach(steps) { step in
                        let binding = self.binding(for: step)
                        HStack {
                            #if os(macOS)
                            Button {
                                recording = nil
                                steps.removeAll { $0.id == step.id }
                            } label: {
                                Image(systemName: "minus.circle.fill")
                                    .symbolRenderingMode(.palette)
                                    .foregroundStyle(.white, .red)
                                    .font(.title2)
                            }
                            .buttonStyle(.plain)
                            .accessibilityLabel(Text("Delete"))
                            #endif
                            if step.sendsText {
                                if #available(iOS 16, *) {
                                    TextField("Text", text: binding.text, prompt: Text("Text"), axis: .vertical)
                                        .labelsHidden()
                                } else {
                                    TextField("Text", text: binding.text)
                                }
                            } else {
                                if step.shortcut.command == nil {
                                    ForEach(modifiers, id: \.label) { modifier in
                                        Toggle(isOn: Binding(
                                            get: { step.shortcut.eventModifiers.contains(modifier.flag) },
                                            set: { on in
                                                var flags = step.shortcut.eventModifiers
                                                if on { flags.insert(modifier.flag) } else { flags.remove(modifier.flag) }
                                                binding.wrappedValue.shortcut = KeyShortcut(key: step.shortcut.key, modifiers: flags)
                                            }
                                        )) {
                                            Text(verbatim: modifier.label)
                                        }
                                        .toggleStyle(.button)
                                        .accessibilityLabel(Text(verbatim: modifier.title))
                                    }
                                }
                                keyControl(binding)
                                Spacer(minLength: 0)
                            }
                            #if os(macOS)
                            Image(systemName: "line.3.horizontal")
                                .foregroundStyle(.tertiary)
                                .accessibilityHidden(true)
                            #endif
                        }
                        #if os(macOS)
                        .contextMenu {
                            Button {
                                guard let index = steps.firstIndex(where: { $0.id == step.id }), index > 0 else { return }
                                steps.swapAt(index, index - 1)
                            } label: {
                                Label("Move Up", systemImage: "arrow.up")
                            }
                            .disabled(steps.first?.id == step.id)
                            Button {
                                guard let index = steps.firstIndex(where: { $0.id == step.id }), index < steps.count - 1 else { return }
                                steps.swapAt(index, index + 1)
                            } label: {
                                Label("Move Down", systemImage: "arrow.down")
                            }
                            .disabled(steps.last?.id == step.id)
                        }
                        #endif
                    }
                    .onMove { source, destination in steps.move(fromOffsets: source, toOffset: destination) }
                    .onDelete { offsets in
                        recording = nil
                        steps.remove(atOffsets: offsets)
                    }
                    Menu {
                        Button {
                            steps.append(Step(.key(.unset)))
                        } label: {
                            Label("Key", systemImage: "keyboard")
                        }
                        Button {
                            steps.append(Step(.text("")))
                        } label: {
                            Label("Text", systemImage: "character.cursor.ibeam")
                        }
                    } label: {
                        HStack {
                            Image(systemName: "plus.circle.fill")
                                .symbolRenderingMode(.palette)
                                .foregroundStyle(.white, .green)
                                .font(.title2)
                                .accessibilityHidden(true)
                            Text("Add")
                                .foregroundStyle(.primary)
                            Spacer(minLength: 0)
                        }
                        .contentShape(Rectangle())
                    }
                    #if os(macOS)
                    .menuStyle(.button)
                    .buttonStyle(.plain)
                    .menuIndicator(.hidden)
                    #endif
                }
            }
            #if os(iOS)
            .environment(\.editMode, .constant(.active))
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done", action: save)
                        .disabled(!isComplete)
                }
            }
        }
        .alert($alert)
        .modifier(ShortcutRecorder(recording: $recording, onPress: record))
        #if os(macOS)
        .frame(minWidth: 440, minHeight: 400)
        #endif
    }

    @ViewBuilder
    private func keyControl(_ step: Binding<Step>) -> some View {
        let shortcut = step.wrappedValue.shortcut
        let key = shortcut.isUnset ? nil : KeyShortcut(key: shortcut.key, modifiers: []).displayName(for: target)
        let isRecording = recording == .key(step.id)
        Menu {
            Button {
                recording = .key(step.id)
            } label: {
                Label("Type Key", systemImage: "keyboard")
            }
            ForEach(KeyShortcut.KeyGroup.allCases) { group in
                let sections = group.sections(for: target)
                if !sections.isEmpty {
                    Menu(group.title) {
                        ForEach(sections) { section in
                            Section {
                                ForEach(section.keys) { key in
                                    Button {
                                        recording = nil
                                        step.wrappedValue.shortcut = KeyShortcut(key: key.name, modifiers: key.command == nil ? shortcut.eventModifiers : [])
                                    } label: {
                                        Text(verbatim: key.label(for: target))
                                    }
                                }
                            } header: {
                                if let title = section.title {
                                    Text(verbatim: title)
                                }
                            }
                        }
                    }
                }
            }
        } label: {
            ShortcutButtonLabel(isRecording: isRecording, shortcut: key, prompt: "Type Key")
        }
        .buttonStyle(.bordered)
        .tint(isRecording ? .accentColor : nil)
        .id(isRecording)
        .accessibilityLabel(Text("Key"))
        .accessibilityValue(isRecording ? Text("Type Key") : key.map { Text(verbatim: $0) } ?? Text("None"))
    }

    private func record(_ field: Field, _ press: ShortcutPress) -> Bool {
        switch press {
        case .cancel:
            recording = nil
        case .clear:
            recording = nil
            switch field {
            case .trigger:
                trigger = .unset
            case .key(let stepID):
                guard let index = steps.firstIndex(where: { $0.id == stepID }) else { break }
                steps[index].shortcut = KeyShortcut(key: "", modifiers: steps[index].shortcut.eventModifiers)
            }
        case .shortcut(let shortcut):
            switch field {
            case .trigger:
                guard shortcut.isValidTrigger else { return false }
                recording = nil
                trigger = shortcut
            case .key(let stepID):
                guard shortcut.chord(for: target) != nil else { return false }
                recording = nil
                guard let index = steps.firstIndex(where: { $0.id == stepID }) else { break }
                let modifiers = shortcut.eventModifiers.isEmpty ? steps[index].shortcut.eventModifiers : shortcut.eventModifiers
                steps[index].shortcut = KeyShortcut(key: shortcut.key, modifiers: modifiers)
            }
        #if os(macOS)
        case .modifierChord:
            return false
        #endif
        }
        return true
    }

    private func save() {
        let shortcut = trigger
        if let other = shortcuts.action(using: shortcut) {
            alert = AlertState(
                title: String(localized: "“\(shortcut.displayName)” Is Already in Use"),
                message: String(localized: "It’s assigned to “\(other.title)”. Replacing it removes that shortcut."),
                actions: [
                    AlertState.Action(label: String(localized: "Cancel"), role: .cancel) {},
                    AlertState.Action(label: String(localized: "Replace"), role: nil, isDefault: true) {
                        shortcuts.set(nil, for: other)
                        commit()
                    },
                ]
            )
        } else if let other = shortcuts.mapping(triggeredBy: shortcut, for: target), other.id != id {
            alert = AlertState(
                title: String(localized: "“\(shortcut.displayName)” Is Already in Use"),
                message: String(localized: "It’s mapped to \(other.displayName(for: target)). Replacing it removes that mapping."),
                actions: [
                    AlertState.Action(label: String(localized: "Cancel"), role: .cancel) {},
                    AlertState.Action(label: String(localized: "Replace"), role: nil, isDefault: true) {
                        shortcuts.removeMapping(other.id, from: target)
                        commit()
                    },
                ]
            )
        } else {
            commit()
        }
    }

    private func commit() {
        if isNew {
            shortcuts.add(mapping, to: target)
        } else {
            shortcuts.update(mapping, in: target)
        }
        dismiss()
    }
}

#if os(iOS)
@Perceptible
private final class ConnectedKeyboard {
    static let shared = ConnectedKeyboard()

    private(set) var isConnected = GCKeyboard.coalesced != nil

    private init() {
        NotificationCenter.default.addObserver(forName: .GCKeyboardDidConnect, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { ConnectedKeyboard.shared.isConnected = true }
        }
        NotificationCenter.default.addObserver(forName: .GCKeyboardDidDisconnect, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { ConnectedKeyboard.shared.isConnected = false }
        }
    }
}
#endif

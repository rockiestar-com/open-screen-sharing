import SwiftUI

struct SheetForm<Content: View>: View {
    private let title: Text
    private let content: Content

    init(_ title: Text, @ViewBuilder content: () -> Content) {
        self.title = title
        self.content = content()
    }

    var body: some View {
        #if os(iOS)
        Form {
            content
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        #else
        Form {
            Section {
            } header: {
                title
                    .font(.title3.bold())
            }
            content
        }
        .formStyle(.grouped)
        .toolbar(.hidden, for: .windowToolbar)
        #endif
    }
}

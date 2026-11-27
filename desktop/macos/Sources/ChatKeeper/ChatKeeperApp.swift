import AppKit
import ChatKeeperKit
import SwiftUI

@main
struct ChatKeeperApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: delegate.model)
        } label: {
            StatusLabel(model: delegate.model)
        }
        .menuBarExtraStyle(.menu)
    }
}

struct StatusLabel: View {
    @ObservedObject var model: AppModel

    var body: some View {
        Image(systemName: model.indicator.symbolName)
            .accessibilityLabel("ChatKeeper: \(model.indicator.title)")
    }
}

struct MenuContent: View {
    @ObservedObject var model: AppModel

    var body: some View {
        Text(model.statusLine)
        if let detail = model.detailLine {
            Text(detail)
        }
        if let error = model.lastError {
            Text(error)
        }
        Divider()
        Button("Open ChatKeeper") { model.openChatKeeper() }
            .keyboardShortcut("o")
            .disabled(!model.canOpen)
        if !model.recent.isEmpty {
            Divider()
            Text("Recent Archives")
            ForEach(model.recent) { archive in
                Button(recentLabel(archive)) { model.openChatKeeper(path: archive.path) }
                    .disabled(!model.canOpen)
            }
        }
        Divider()
        if let status = model.status, !status.workspaces.isEmpty {
            Button(status.paused ? "Resume Capture" : "Pause Capture") { model.togglePause() }
        }
        switch model.phase {
        case .stopped, .failed:
            Button("Start ChatKeeper") { model.restartServer() }
        case .running(external: true):
            EmptyView()
        default:
            Button("Restart ChatKeeper") { model.restartServer() }
        }
        Toggle("Start at Login", isOn: Binding(get: { model.loginItemEnabled }, set: { model.setLoginItem($0) }))
        Button("Check for Updates…") { model.updater.checkForUpdates() }
        Button("Show Logs") { model.openLogs() }
        Divider()
        Button("Quit ChatKeeper") { NSApp.terminate(nil) }
            .keyboardShortcut("q")
    }

    private func recentLabel(_ archive: DesktopStatus.Archive) -> String {
        guard let date = archive.date else { return archive.title }
        return "\(archive.title) — \(AppModel.relative.localizedString(for: date, relativeTo: Date()))"
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = AppModel()

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Single instance per user: hand over to the copy already running.
        if let id = Bundle.main.bundleIdentifier {
            let others = NSRunningApplication.runningApplications(withBundleIdentifier: id)
                .filter { $0.processIdentifier != ProcessInfo.processInfo.processIdentifier }
            if let other = others.first {
                other.activate(options: [])
                NSApp.terminate(nil)
                return
            }
        }
        model.start()
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        model.shutdown {
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }
}

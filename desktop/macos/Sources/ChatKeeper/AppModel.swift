import AppKit
import ChatKeeperKit
import Foundation
import ServiceManagement

@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var phase: ServerPhase = .stopped
    @Published private(set) var healthy = false
    @Published private(set) var status: DesktopStatus?
    @Published private(set) var loginItemEnabled = false
    @Published var lastError: String?

    let updater = Updater()
    private let supervisor = ServerSupervisor()
    private var launch: LaunchInfo?
    private var restart = RestartPolicy()
    private var pollTask: Task<Void, Never>?
    private var restartWork: DispatchWorkItem?

    var indicator: Indicator { Indicator.from(phase: phase, healthy: healthy, status: status) }
    var canOpen: Bool { launch != nil && healthy }
    var isExternal: Bool { phase == .running(external: true) }
    var recent: [DesktopStatus.Archive] { Array((status?.recent ?? []).prefix(5)) }

    var port: Int {
        let v = UserDefaults.standard.integer(forKey: "ServerPort")
        return (1024...65535).contains(v) ? v : ChildCommand.defaultPort
    }

    var statusLine: String {
        var line = "ChatKeeper — \(indicator.title)"
        if case let .failed(message) = phase { line += ": \(message)" }
        if isExternal { line += " (external)" }
        return line
    }

    var detailLine: String? {
        guard let status else { return nil }
        if status.configError { return "Configuration needs attention" }
        if let failing = status.failingWorkspaces.first {
            return "\(failing.id): \(failing.errorClass ?? failing.state)"
        }
        guard let date = status.lastCaptureDate else {
            return status.workspaces.isEmpty ? "Open ChatKeeper to connect a workspace" : "Waiting for first archive"
        }
        return "Last archive " + Self.relative.localizedString(for: date, relativeTo: Date())
    }

    static let relative: RelativeDateTimeFormatter = {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .short
        return f
    }()

    init() {
        supervisor.onEvent = { [weak self] event in
            Task { @MainActor in self?.handle(event) }
        }
        refreshLoginItem()
    }

    // MARK: Lifecycle

    func start() {
        restartWork?.cancel()
        lastError = nil
        phase = .starting
        healthy = false
        status = nil
        supervisor.start(port: port)
    }

    func restartServer() {
        restart.reset()
        if supervisor.isRunning {
            supervisor.stop { [weak self] in self?.start() }
        } else {
            start()
        }
    }

    /// Stops the child we own; an attached external instance keeps running.
    func shutdown(completion: @escaping () -> Void) {
        restartWork?.cancel()
        pollTask?.cancel()
        supervisor.stop(completion: completion)
    }

    private func handle(_ event: ServerSupervisor.Event) {
        switch event {
        case let .launched(info):
            launch = info
            phase = .running(external: false)
            startPolling()
        case let .attached(info):
            launch = info
            phase = .running(external: true)
            startPolling()
        case let .failedToStart(message):
            phase = .failed(message)
            lastError = message
        case let .exited(code):
            pollTask?.cancel()
            healthy = false
            status = nil
            launch = nil
            if let delay = restart.recordFailure() {
                phase = .starting
                let work = DispatchWorkItem { [weak self] in Task { @MainActor in self?.start() } }
                restartWork = work
                DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: work)
            } else {
                phase = .failed("server exited (\(code)); see ~/Library/Logs/ChatKeeper")
            }
        }
    }

    // MARK: Polling

    private func startPolling() {
        pollTask?.cancel()
        guard let info = launch else { return }
        pollTask = Task { [weak self] in
            let client = StatusClient(info: info)
            var misses = 0
            while !Task.isCancelled {
                let ok = await client.healthy()
                var snapshot: DesktopStatus?
                if ok { snapshot = try? await client.status() }
                guard let self else { return }
                misses = ok ? 0 : misses + 1
                self.healthy = ok || misses < 3
                if let snapshot {
                    self.status = snapshot
                    self.restart.reset()
                }
                // An attached instance may go away; try to take over.
                if self.isExternal && misses >= 3 {
                    self.start()
                    return
                }
                try? await Task.sleep(nanoseconds: (snapshot == nil ? 2 : 5) * 1_000_000_000)
            }
        }
    }

    // MARK: Actions

    func openChatKeeper(path: String? = nil) {
        guard let launch else { return }
        NSWorkspace.shared.open(launch.loginURL(next: path))
    }

    func togglePause() {
        guard let launch, let status else { return }
        let want = !status.paused
        Task {
            do {
                self.status = try await StatusClient(info: launch).setPaused(want)
            } catch {
                self.lastError = "Could not \(want ? "pause" : "resume") capture"
            }
        }
    }

    func refreshLoginItem() {
        loginItemEnabled = SMAppService.mainApp.status == .enabled
    }

    func setLoginItem(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            lastError = nil
        } catch {
            lastError = "Start at login: \(error.localizedDescription)"
        }
        if SMAppService.mainApp.status == .requiresApproval {
            SMAppService.openSystemSettingsLoginItems()
        }
        refreshLoginItem()
    }

    func openLogs() {
        NSWorkspace.shared.activateFileViewerSelecting([ServerSupervisor.logURL])
    }
}

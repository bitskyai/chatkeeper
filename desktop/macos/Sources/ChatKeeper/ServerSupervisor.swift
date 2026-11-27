import AppKit
import ChatKeeperKit
import Foundation

/// Owns the bundled `chatkeeper` child process: starts it in desktop mode,
/// reads the per-launch URL from stdout, and stops it cleanly on quit.
final class ServerSupervisor {
    enum Event {
        /// We started the server and it printed its launch URL.
        case launched(LaunchInfo)
        /// Another process holds the instance lock; we attached to it.
        case attached(LaunchInfo)
        /// The child exited without being asked to (status code).
        case exited(Int32)
        case failedToStart(String)
    }

    var onEvent: ((Event) -> Void)?
    private var process: Process?
    private var stopping = false
    private var attachedToExisting = false
    private let queue = DispatchQueue(label: "ai.bitsky.chatkeeper.supervisor")
    private var log: FileHandle?
    /// Accessed only on `queue`.
    private var stdoutLines = LineBuffer()

    var isRunning: Bool { process?.isRunning ?? false }

    static var binaryURL: URL? {
        Bundle.main.url(forResource: "chatkeeper", withExtension: nil)
    }

    static var logURL: URL {
        let logs = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask).first!
            .appendingPathComponent("Logs/ChatKeeper", isDirectory: true)
        try? FileManager.default.createDirectory(at: logs, withIntermediateDirectories: true)
        return logs.appendingPathComponent("chatkeeper.log")
    }

    func start(port: Int) {
        guard process == nil else { return }
        guard let binary = Self.binaryURL else {
            emit(.failedToStart("The bundled chatkeeper binary is missing. Reinstall ChatKeeper."))
            return
        }
        stopping = false
        attachedToExisting = false
        openLog()

        let child = Process()
        child.executableURL = binary
        child.arguments = ChildCommand.arguments(port: port)
        var env = ProcessInfo.processInfo.environment
        env["CHATKEEPER_LAUNCHER"] = "menubar"
        child.environment = env
        let stdout = Pipe()
        let stderr = Pipe()
        child.standardOutput = stdout
        child.standardError = stderr
        child.standardInput = FileHandle.nullDevice

        stdoutLines = LineBuffer()
        stdout.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                return
            }
            self?.queue.async {
                guard let self else { return }
                for line in self.stdoutLines.append(data) { self.handleStdout(line) }
            }
        }
        stderr.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                return
            }
            self?.queue.async { self?.writeLog(String(decoding: data, as: UTF8.self)) }
        }
        child.terminationHandler = { [weak self] p in
            self?.queue.async { self?.handleExit(p) }
        }
        do {
            try child.run()
            process = child
            let pid = child.processIdentifier
            queue.async { [weak self] in self?.writeLog("started chatkeeper pid \(pid)\n") }
        } catch {
            emit(.failedToStart("Could not start chatkeeper: \(error.localizedDescription)"))
        }
    }

    /// SIGTERM, then SIGKILL after `timeout`. Completion runs on the main queue.
    func stop(timeout: TimeInterval = 10, completion: @escaping () -> Void) {
        guard let child = process, child.isRunning else {
            process = nil
            DispatchQueue.main.async(execute: completion)
            return
        }
        stopping = true
        child.terminate()
        DispatchQueue.global().async {
            let deadline = Date().addingTimeInterval(timeout)
            while child.isRunning && Date() < deadline {
                Thread.sleep(forTimeInterval: 0.1)
            }
            if child.isRunning {
                kill(child.processIdentifier, SIGKILL)
                child.waitUntilExit()
            }
            DispatchQueue.main.async(execute: completion)
        }
    }

    // Runs on `queue`; state changes hop to the main queue.
    private func handleStdout(_ line: String) {
        writeLog(redactSecrets(line) + "\n")
        if let info = LaunchInfo.parse(line: line) {
            emit(.launched(info))
        } else if line.contains(ChildCommand.alreadyRunningMarker) {
            DispatchQueue.main.async { [weak self] in self?.attachToExisting() }
        }
    }

    private func handleExit(_ p: Process) {
        writeLog("chatkeeper exited with status \(p.terminationStatus)\n")
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            if self.process === p { self.process = nil }
            if self.stopping { return }
            // Exit 0 without a stop request means another instance holds the
            // lock (stdout may not have drained yet, so check the file too).
            if p.terminationStatus == 0 {
                if !self.attachedToExisting { self.attachToExisting() }
                return
            }
            self.emit(.exited(p.terminationStatus))
        }
    }

    private func attachToExisting() {
        guard !attachedToExisting else { return }
        attachedToExisting = true
        if let home = ChildCommand.home(), let info = ChildCommand.instanceURL(home: home) {
            emit(.attached(info))
        } else {
            emit(.failedToStart("Another ChatKeeper is running but its address is unavailable."))
        }
    }

    private func emit(_ event: Event) {
        DispatchQueue.main.async { [weak self] in self?.onEvent?(event) }
    }

    private func openLog() {
        let url = Self.logURL
        if !FileManager.default.fileExists(atPath: url.path) {
            FileManager.default.createFile(atPath: url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        log = try? FileHandle(forWritingTo: url)
        log?.seekToEndOfFile()
    }

    private func writeLog(_ text: String) {
        log?.write(Data(redactSecrets(text).utf8))
    }
}

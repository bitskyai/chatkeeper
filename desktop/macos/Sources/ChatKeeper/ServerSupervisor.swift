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
    /// Write end of the child's stdin. Never written; held open so the child
    /// sees EOF (and shuts down) as soon as this app dies for any reason.
    private var lifeline: FileHandle?
    private var stopping = false
    private var attachedToExisting = false
    private let queue = DispatchQueue(label: "ai.bitsky.chatkeeper.supervisor")
    private var log: FileHandle?
    /// Accessed only on `queue`.
    private var stdoutLines = LineBuffer()
    private var stderrLines = LineBuffer()

    var isRunning: Bool { process?.isRunning ?? false }

    static var binaryURL: URL? {
        Bundle.main.url(forResource: "chatkeeper", withExtension: nil)
    }

    static var logURL: URL {
        let logs = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask).first!
            .appendingPathComponent("Logs/ChatKeeper", isDirectory: true)
        try? FileManager.default.createDirectory(at: logs, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        return logs.appendingPathComponent("chatkeeper.log")
    }

    /// Where the pid of the server this app started is recorded.
    static var ownedChildURL: URL? {
        ChildCommand.home().map(OwnedChild.url(home:))
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
        stopStaleOwnedChild()

        let child = Process()
        child.executableURL = binary
        child.arguments = ChildCommand.arguments(port: port)
        child.environment = ChildCommand.environment()
        let stdout = Pipe()
        let stderr = Pipe()
        let stdin = Pipe()
        child.standardOutput = stdout
        child.standardError = stderr
        child.standardInput = stdin

        stdoutLines = LineBuffer()
        stderrLines = LineBuffer()
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
                self?.queue.async {
                    guard let self else { return }
                    for line in self.stderrLines.append(Data([0x0A])) { self.writeLog(line + "\n") }
                }
                return
            }
            self?.queue.async {
                guard let self else { return }
                for line in self.stderrLines.append(data) { self.writeLog(line + "\n") }
            }
        }
        child.terminationHandler = { [weak self] p in
            self?.queue.async { self?.handleExit(p) }
        }
        do {
            try child.run()
            lifeline = stdin.fileHandleForWriting
            process = child
            let pid = child.processIdentifier
            if let url = Self.ownedChildURL, let identity = ProcessProbe.identity(pid: pid) {
                try? OwnedChild(pid: pid, identity: identity).save(to: url)
            }
            queue.async { [weak self] in self?.writeLog("started chatkeeper pid \(pid)\n") }
        } catch {
            emit(.failedToStart("Could not start chatkeeper: \(error.localizedDescription)"))
        }
    }

    /// SIGTERM, then SIGKILL after `timeout`. Completion runs on the main queue.
    /// Call on the main queue. `process` is cleared before `completion`, so
    /// a restart from the completion can start a new child immediately.
    func stop(timeout: TimeInterval = 10, completion: @escaping () -> Void) {
        guard let child = process, child.isRunning else {
            if let child = process { forget(child) }
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
            DispatchQueue.main.async { [weak self] in
                self?.forget(child)
                completion()
            }
        }
    }

    /// Main queue: drop all state for `child` if it is still the current one.
    private func forget(_ child: Process) {
        guard process === child else { return }
        process = nil
        try? lifeline?.close()
        lifeline = nil
        if let url = Self.ownedChildURL { OwnedChild.clear(at: url, pid: child.processIdentifier) }
    }

    /// A previous app instance that crashed or was force-quit may have left
    /// its server running (holding the instance lock). If the recorded child
    /// is still that exact bundled server, stop it so this instance owns the
    /// server again instead of attaching to an orphan it cannot quit.
    private func stopStaleOwnedChild() {
        guard let url = Self.ownedChildURL, let record = OwnedChild.load(from: url) else { return }
        defer { try? FileManager.default.removeItem(at: url) }
        guard record.isStillOurs(current: ProcessProbe.identity(pid: record.pid)) else { return }
        writeLog("stopping stale chatkeeper pid \(record.pid) left by a previous app instance\n")
        kill(record.pid, SIGTERM)
        let deadline = Date().addingTimeInterval(5)
        while Date() < deadline, record.isStillOurs(current: ProcessProbe.identity(pid: record.pid)) {
            Thread.sleep(forTimeInterval: 0.1)
        }
        if record.isStillOurs(current: ProcessProbe.identity(pid: record.pid)) {
            kill(record.pid, SIGKILL)
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
            // A child replaced by a restart (or already forgotten by stop) is
            // stale: its exit must not trigger restart or attach logic.
            guard self.process === p else { return }
            self.forget(p)
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
        guard log == nil else { return } // opened once; queued writes may still use it
        let url = Self.logURL
        if !FileManager.default.fileExists(atPath: url.path) {
            FileManager.default.createFile(atPath: url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        log = try? FileHandle(forWritingTo: url)
        log?.seekToEndOfFile()
    }

    private func writeLog(_ text: String) {
        let data = Data(redactSecrets(text).utf8)
        // Close before rotation, then reopen the active file on the serial queue.
        try? log?.close()
        log = nil
        do {
            try rotateLog(at: Self.logURL, incoming: data.count)
            openLog()
            try log?.write(contentsOf: data)
        } catch {
            // Logging failure must not kill capture; retry opening on the next write.
            log = nil
        }
    }
}

/// Live identity of a process via libproc (path + kernel start time).
enum ProcessProbe {
    static func identity(pid: Int32) -> ProcessIdentity? {
        guard pid > 0 else { return nil }
        var info = proc_bsdinfo()
        let size = Int32(MemoryLayout<proc_bsdinfo>.size)
        guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size else { return nil }
        var path = [CChar](repeating: 0, count: 4 * Int(MAXPATHLEN))
        guard proc_pidpath(pid, &path, UInt32(path.count)) > 0 else { return nil }
        return ProcessIdentity(executable: String(cString: path),
                               startMicros: info.pbi_start_tvsec * 1_000_000 + info.pbi_start_tvusec)
    }
}

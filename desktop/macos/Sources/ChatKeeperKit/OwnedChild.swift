import Foundation

/// Identity of a live process: its executable path and kernel start time.
/// Together they rule out pid reuse.
public struct ProcessIdentity: Codable, Equatable, Sendable {
    public let executable: String
    /// Start time in microseconds since 1970 (`pbi_start_tvsec`/`tvusec`).
    public let startMicros: UInt64

    public init(executable: String, startMicros: UInt64) {
        self.executable = executable
        self.startMicros = startMicros
    }
}

/// The `chatkeeper` child the menu-bar app started, recorded on disk so a
/// relaunch after a crash or force-quit can stop a server the previous app
/// instance owned (instead of attaching to it as "external" and leaving it
/// running after Quit). The server also exits by itself when its launcher
/// dies (launcher mode); this is the second line of defence.
public struct OwnedChild: Codable, Equatable, Sendable {
    public static let fileName = "desktop-child.json"
    /// Path suffix every bundled server binary has, wherever the app lives
    /// (Gatekeeper App Translocation can move the bundle between launches).
    public static let bundledSuffix = "/Contents/Resources/chatkeeper"

    public let pid: Int32
    public let identity: ProcessIdentity

    public init(pid: Int32, identity: ProcessIdentity) {
        self.pid = pid
        self.identity = identity
    }

    public static func url(home: URL) -> URL {
        home.appendingPathComponent(fileName)
    }

    public static func load(from url: URL) -> OwnedChild? {
        guard let data = try? Data(contentsOf: url) else { return nil }
        return try? JSONDecoder().decode(OwnedChild.self, from: data)
    }

    public func save(to url: URL) throws {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        let data = try JSONEncoder().encode(self)
        try data.write(to: url, options: [.atomic])
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }

    /// Removes the record only if it still describes `pid` (a newer child
    /// may have replaced it).
    public static func clear(at url: URL, pid: Int32) {
        guard let current = load(from: url), current.pid == pid else { return }
        try? FileManager.default.removeItem(at: url)
    }

    /// True when the recorded process is still alive and is the same
    /// bundled server we started: same start time and a bundled binary path.
    /// `current` is the live identity of `pid` (nil if no such process).
    public func isStillOurs(current: ProcessIdentity?) -> Bool {
        guard pid > 1, let current else { return false }
        return current.startMicros == identity.startMicros
            && current.executable == identity.executable
            && current.executable.hasSuffix(Self.bundledSuffix)
    }
}

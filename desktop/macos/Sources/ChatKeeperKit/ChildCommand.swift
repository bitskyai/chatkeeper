import Foundation

/// How the menu-bar app launches the bundled `chatkeeper` binary.
public enum ChildCommand {
    public static let defaultPort = 8787
    /// Printed by the CLI when another process already holds the instance lock.
    public static let alreadyRunningMarker = "already running"

    /// Desktop mode on a fixed loopback port. If the port is busy the server
    /// falls back to an ephemeral one and prints the actual URL on stdout.
    public static func arguments(port: Int = defaultPort) -> [String] {
        ["run", "--mode", "desktop", "--no-browser", "--bind", "127.0.0.1:\(port)"]
    }

    /// Same resolution as the CLI: $CHATKEEPER_HOME, else
    /// <UserConfigDir>/chatkeeper (~/Library/Application Support/chatkeeper).
    public static func home(environment: [String: String] = ProcessInfo.processInfo.environment,
                            applicationSupport: URL? = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first) -> URL? {
        if let v = environment["CHATKEEPER_HOME"], !v.isEmpty {
            return URL(fileURLWithPath: v, isDirectory: true)
        }
        return applicationSupport?.appendingPathComponent("chatkeeper", isDirectory: true)
    }

    /// The running instance's launch URL (written 0600 by the server).
    public static func instanceURL(home: URL) -> LaunchInfo? {
        let file = home.appendingPathComponent("instance.url")
        guard let text = try? String(contentsOf: file, encoding: .utf8) else { return nil }
        return LaunchInfo(string: text)
    }
}

/// Splits a byte stream into lines, keeping any partial trailing line.
public struct LineBuffer: Sendable {
    private var pending = Data()
    public init() {}

    public mutating func append(_ data: Data) -> [String] {
        pending.append(data)
        var lines: [String] = []
        while let i = pending.firstIndex(of: 0x0A) {
            let line = pending[pending.startIndex..<i]
            lines.append(String(decoding: line, as: UTF8.self).trimmingCharacters(in: CharacterSet(charactersIn: "\r")))
            pending.removeSubrange(pending.startIndex...i)
        }
        return lines
    }
}

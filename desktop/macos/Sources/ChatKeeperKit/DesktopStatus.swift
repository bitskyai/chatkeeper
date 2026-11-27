import Foundation

/// Mirrors the JSON from `GET /api/desktop/status`. Contains identifiers,
/// states and timestamps only — never credentials or message content.
public struct DesktopStatus: Codable, Equatable, Sendable {
    public struct Workspace: Codable, Equatable, Sendable {
        public var id: String
        public var plugin: String
        public var state: String
        public var errorClass: String?
        public var realtime: Bool
        public var paused: Bool
        public var lastCapture: String?

        enum CodingKeys: String, CodingKey {
            case id, plugin, state, realtime, paused
            case errorClass = "error_class"
            case lastCapture = "last_capture"
        }
    }

    public struct Archive: Codable, Equatable, Sendable, Identifiable {
        public var workspace: String
        public var channel: String
        public var root: String
        public var path: String
        public var at: String

        public var id: String { path }
        public var date: Date? { parseTimestamp(at) }
        public var title: String { "\(workspace) · \(channel)" }
    }

    /// One of: setup, running, capturing, paused, error.
    public var state: String
    public var configError: Bool
    public var paused: Bool
    public var lastCapture: String?
    public var workspaces: [Workspace]
    public var recent: [Archive]

    enum CodingKeys: String, CodingKey {
        case state, paused, workspaces, recent
        case configError = "config_error"
        case lastCapture = "last_capture"
    }

    public init(state: String, configError: Bool = false, paused: Bool = false, lastCapture: String? = nil, workspaces: [Workspace] = [], recent: [Archive] = []) {
        self.state = state
        self.configError = configError
        self.paused = paused
        self.lastCapture = lastCapture
        self.workspaces = workspaces
        self.recent = recent
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = try c.decode(String.self, forKey: .state)
        configError = try c.decodeIfPresent(Bool.self, forKey: .configError) ?? false
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? false
        lastCapture = try c.decodeIfPresent(String.self, forKey: .lastCapture)
        workspaces = try c.decodeIfPresent([Workspace].self, forKey: .workspaces) ?? []
        recent = try c.decodeIfPresent([Archive].self, forKey: .recent) ?? []
    }

    public static func decode(_ data: Data) throws -> DesktopStatus {
        try JSONDecoder().decode(DesktopStatus.self, from: data)
    }

    public var lastCaptureDate: Date? { lastCapture.flatMap(parseTimestamp) }
    public var failingWorkspaces: [Workspace] {
        workspaces.filter { $0.state == "failed" || $0.state == "quarantined" }
    }
}

func parseTimestamp(_ s: String) -> Date? {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime]
    return f.date(from: s)
}

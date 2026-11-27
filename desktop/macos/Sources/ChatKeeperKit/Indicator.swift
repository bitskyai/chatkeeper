import Foundation

/// Lifecycle of the bundled server as seen by the menu-bar app.
public enum ServerPhase: Equatable, Sendable {
    case stopped
    case starting
    /// `external` means another process (CLI, `brew services`) owns the
    /// instance lock and we only attached to it; Quit leaves it running.
    case running(external: Bool)
    case failed(String)
}

/// What the status item shows.
public enum Indicator: String, Equatable, Sendable, CaseIterable {
    case starting, running, capturing, paused, setup, error, stopped

    /// SF Symbol rendered as a template image in the menu bar.
    public var symbolName: String {
        switch self {
        case .starting: return "tray"
        case .running: return "tray.full"
        case .capturing: return "tray.and.arrow.down.fill"
        case .paused: return "pause.circle"
        case .setup: return "tray.and.arrow.down"
        case .error: return "exclamationmark.triangle.fill"
        case .stopped: return "tray.circle"
        }
    }

    public var title: String {
        switch self {
        case .starting: return "Starting…"
        case .running: return "Running"
        case .capturing: return "Capturing"
        case .paused: return "Capture paused"
        case .setup: return "Needs setup"
        case .error: return "Needs attention"
        case .stopped: return "Stopped"
        }
    }

    /// Combines process phase, the last `/healthz` probe and the last status.
    public static func from(phase: ServerPhase, healthy: Bool, status: DesktopStatus?) -> Indicator {
        switch phase {
        case .stopped: return .stopped
        case .failed: return .error
        case .starting: return .starting
        case .running:
            guard healthy else { return .error }
            guard let status else { return .starting }
            switch status.state {
            case "capturing": return .capturing
            case "paused": return .paused
            case "setup": return .setup
            case "error": return .error
            default: return .running
            }
        }
    }
}

/// Restart policy for an unexpectedly exited child: exponential backoff,
/// giving up after `maxFailures` exits inside `window`.
public struct RestartPolicy: Sendable {
    public var maxFailures: Int
    public var window: TimeInterval
    public var baseDelay: TimeInterval
    public var maxDelay: TimeInterval
    private(set) var failures: [Date] = []

    public init(maxFailures: Int = 5, window: TimeInterval = 300, baseDelay: TimeInterval = 2, maxDelay: TimeInterval = 60) {
        self.maxFailures = maxFailures
        self.window = window
        self.baseDelay = baseDelay
        self.maxDelay = maxDelay
    }

    /// Records a failure; returns the delay before restarting, or nil to give up.
    public mutating func recordFailure(at now: Date = Date()) -> TimeInterval? {
        failures = failures.filter { now.timeIntervalSince($0) < window } + [now]
        if failures.count >= maxFailures { return nil }
        return min(maxDelay, baseDelay * pow(2, Double(failures.count - 1)))
    }

    public mutating func reset() { failures.removeAll() }
}

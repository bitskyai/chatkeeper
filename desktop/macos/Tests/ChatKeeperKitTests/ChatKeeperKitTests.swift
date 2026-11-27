import XCTest
@testable import ChatKeeperKit

final class LaunchInfoTests: XCTestCase {
    func testParsesStdoutLine() throws {
        let info = try XCTUnwrap(LaunchInfo.parse(line: "Open ChatKeeper: http://127.0.0.1:8787/login?token=abc_DEF-123"))
        XCTAssertEqual(info.token, "abc_DEF-123")
        XCTAssertEqual(info.baseURL.absoluteString, "http://127.0.0.1:8787")
        XCTAssertEqual(info.endpoint("/api/desktop/status").absoluteString, "http://127.0.0.1:8787/api/desktop/status")
        XCTAssertEqual(info.endpoint("healthz").absoluteString, "http://127.0.0.1:8787/healthz")
    }

    func testRejectsNonLoopbackAndMalformed() {
        XCTAssertNil(LaunchInfo.parse(line: "ChatKeeper listening on http://0.0.0.0:8787"))
        XCTAssertNil(LaunchInfo(string: "http://evil.example/login?token=x"))
        XCTAssertNil(LaunchInfo(string: "https://127.0.0.1/login?token=x"))
        XCTAssertNil(LaunchInfo(string: "http://127.0.0.1:8787/other?token=x"))
        XCTAssertNil(LaunchInfo(string: "http://127.0.0.1:8787/login"))
        XCTAssertNil(LaunchInfo(string: "http://127.0.0.1:8787/login?token="))
        XCTAssertNotNil(LaunchInfo(string: "http://localhost:9000/login?token=x\n"))
        XCTAssertNotNil(LaunchInfo(string: "http://[::1]:9000/login?token=x"))
        XCTAssertNotNil(LaunchInfo(string: "http://127.8.9.10:9000/login?token=x"))
        // Hostnames that only look like loopback literals.
        XCTAssertNil(LaunchInfo(string: "http://127.0.0.1.evil.example/login?token=x"))
        XCTAssertNil(LaunchInfo(string: "http://127.evil.example/login?token=x"))
        XCTAssertNil(LaunchInfo(string: "http://[::2]:9000/login?token=x"))
        XCTAssertNil(LaunchInfo(string: "http://128.0.0.1/login?token=x"))
    }

    func testLoopbackLiterals() {
        XCTAssertTrue(LaunchInfo.isLoopback("127.0.0.1"))
        XCTAssertTrue(LaunchInfo.isLoopback("127.255.255.254"))
        XCTAssertTrue(LaunchInfo.isLoopback("[::1]"))
        XCTAssertTrue(LaunchInfo.isLoopback("LOCALHOST"))
        XCTAssertFalse(LaunchInfo.isLoopback("127.0.0.1.nip.io"))
        XCTAssertFalse(LaunchInfo.isLoopback("127.1"))
        XCTAssertFalse(LaunchInfo.isLoopback("10.0.0.1"))
        XCTAssertFalse(LaunchInfo.isLoopback("::"))
        XCTAssertFalse(LaunchInfo.isLoopback(""))
    }

    func testLoginURLWithNext() throws {
        let info = try XCTUnwrap(LaunchInfo(string: "http://127.0.0.1:8787/login?token=t"))
        let url = info.loginURL(next: "/view/fixture/CFIX/1.000001")
        let items = try XCTUnwrap(URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems)
        XCTAssertEqual(items.first { $0.name == "token" }?.value, "t")
        XCTAssertEqual(items.first { $0.name == "next" }?.value, "/view/fixture/CFIX/1.000001")
        XCTAssertEqual(info.loginURL(next: nil), info.launchURL)
        XCTAssertEqual(info.loginURL(next: "https://evil.example"), info.launchURL)
    }

    func testRedactsTokens() {
        XCTAssertEqual(redactSecrets("Open ChatKeeper: http://127.0.0.1:1/login?token=SECRET&next=/"), "Open ChatKeeper: http://127.0.0.1:1/login?token=REDACTED&next=/")
        XCTAssertEqual(redactSecrets("no secrets here"), "no secrets here")
    }
}

final class StatusTests: XCTestCase {
    let sample = """
    {"state":"capturing","config_error":false,"paused":false,"last_capture":"2026-11-27T08:00:00Z",
     "workspaces":[{"id":"home","plugin":"slack","state":"running","realtime":true,"paused":false,"last_capture":"2026-11-27T08:00:00Z"},
                   {"id":"work","plugin":"slack","state":"quarantined","error_class":"timeout","realtime":false,"paused":false}],
     "recent":[{"workspace":"home","channel":"C1","root":"1.000001","path":"/view/home/C1/1.000001","at":"2026-11-27T08:00:00Z"}]}
    """

    func testDecodesServerJSON() throws {
        let s = try DesktopStatus.decode(Data(sample.utf8))
        XCTAssertEqual(s.state, "capturing")
        XCTAssertEqual(s.workspaces.count, 2)
        XCTAssertEqual(s.failingWorkspaces.map(\.id), ["work"])
        XCTAssertEqual(s.workspaces[1].errorClass, "timeout")
        XCTAssertEqual(s.recent.first?.title, "home · C1")
        XCTAssertNotNil(s.recent.first?.date)
        XCTAssertEqual(s.lastCaptureDate, ISO8601DateFormatter().date(from: "2026-11-27T08:00:00Z"))
    }

    func testDecodesMinimalJSON() throws {
        let s = try DesktopStatus.decode(Data(#"{"state":"setup"}"#.utf8))
        XCTAssertEqual(s, DesktopStatus(state: "setup"))
    }
}

final class IndicatorTests: XCTestCase {
    func testMapping() {
        let running = ServerPhase.running(external: false)
        XCTAssertEqual(Indicator.from(phase: .stopped, healthy: false, status: nil), .stopped)
        XCTAssertEqual(Indicator.from(phase: .failed("x"), healthy: false, status: nil), .error)
        XCTAssertEqual(Indicator.from(phase: .starting, healthy: false, status: nil), .starting)
        XCTAssertEqual(Indicator.from(phase: running, healthy: false, status: DesktopStatus(state: "running")), .error)
        XCTAssertEqual(Indicator.from(phase: running, healthy: true, status: nil), .starting)
        for (state, want) in [("running", Indicator.running), ("capturing", .capturing), ("paused", .paused), ("setup", .setup), ("error", .error), ("future", .running)] {
            XCTAssertEqual(Indicator.from(phase: running, healthy: true, status: DesktopStatus(state: state)), want, state)
        }
        for i in Indicator.allCases {
            XCTAssertFalse(i.symbolName.isEmpty)
            XCTAssertFalse(i.title.isEmpty)
        }
    }

    func testRestartPolicyBacksOffThenGivesUp() {
        var p = RestartPolicy(maxFailures: 4, window: 60, baseDelay: 2, maxDelay: 5)
        let t = Date(timeIntervalSince1970: 1000)
        XCTAssertEqual(p.recordFailure(at: t), 2)
        XCTAssertEqual(p.recordFailure(at: t + 1), 4)
        XCTAssertEqual(p.recordFailure(at: t + 2), 5)
        XCTAssertNil(p.recordFailure(at: t + 3))
        // Old failures age out of the window.
        XCTAssertEqual(p.recordFailure(at: t + 120), 2)
        p.reset()
        XCTAssertEqual(p.recordFailure(at: t + 121), 2)
    }
}

final class ChildCommandTests: XCTestCase {
    func testArguments() {
        XCTAssertEqual(ChildCommand.arguments(port: 9000), ["run", "--mode", "desktop", "--no-browser", "--bind", "127.0.0.1:9000"])
    }

    func testHomeResolution() {
        let support = URL(fileURLWithPath: "/Users/x/Library/Application Support", isDirectory: true)
        XCTAssertEqual(ChildCommand.home(environment: [:], applicationSupport: support)?.path, "/Users/x/Library/Application Support/chatkeeper")
        XCTAssertEqual(ChildCommand.home(environment: ["CHATKEEPER_HOME": "/tmp/ck"], applicationSupport: support)?.path, "/tmp/ck")
    }

    func testInstanceURL() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        XCTAssertNil(ChildCommand.instanceURL(home: dir))
        try "http://127.0.0.1:8787/login?token=t".write(to: dir.appendingPathComponent("instance.url"), atomically: true, encoding: .utf8)
        XCTAssertEqual(ChildCommand.instanceURL(home: dir)?.token, "t")
    }

    func testLineBuffer() {
        var b = LineBuffer()
        XCTAssertEqual(b.append(Data("Open Chat".utf8)), [])
        XCTAssertEqual(b.append(Data("Keeper: x\r\nsecond\nthi".utf8)), ["Open ChatKeeper: x", "second"])
        XCTAssertEqual(b.append(Data("rd\n".utf8)), ["third"])
    }
}

final class OwnedChildTests: XCTestCase {
    let bundled = "/Applications/ChatKeeper.app/Contents/Resources/chatkeeper"

    func testRoundTripAndClear() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = OwnedChild.url(home: dir)
        XCTAssertNil(OwnedChild.load(from: url))
        let record = OwnedChild(pid: 4242, identity: ProcessIdentity(executable: bundled, startMicros: 1_700_000_000_000_001))
        try record.save(to: url)
        XCTAssertEqual(OwnedChild.load(from: url), record)
        let mode = try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(mode?.intValue, 0o600)
        OwnedChild.clear(at: url, pid: 9999) // a different child: keep
        XCTAssertEqual(OwnedChild.load(from: url), record)
        OwnedChild.clear(at: url, pid: 4242)
        XCTAssertNil(OwnedChild.load(from: url))
    }

    func testStaleOwnedChildDecision() {
        let id = ProcessIdentity(executable: bundled, startMicros: 100)
        let record = OwnedChild(pid: 4242, identity: id)
        // Same process still alive: the previous app instance's server.
        XCTAssertTrue(record.isStillOurs(current: id))
        // Gone, or the pid was reused by another process.
        XCTAssertFalse(record.isStillOurs(current: nil))
        XCTAssertFalse(record.isStillOurs(current: ProcessIdentity(executable: bundled, startMicros: 101)))
        XCTAssertFalse(record.isStillOurs(current: ProcessIdentity(executable: "/bin/zsh", startMicros: 100)))
        // A CLI or brew-services chatkeeper is never "ours".
        let cli = ProcessIdentity(executable: "/opt/homebrew/bin/chatkeeper", startMicros: 100)
        XCTAssertFalse(OwnedChild(pid: 4242, identity: cli).isStillOurs(current: cli))
        // Never signal init/launchd or invalid pids.
        XCTAssertFalse(OwnedChild(pid: 1, identity: id).isStillOurs(current: id))
        XCTAssertFalse(OwnedChild(pid: 0, identity: id).isStillOurs(current: id))
    }

    func testLauncherEnvironment() {
        let env = ChildCommand.environment(base: ["PATH": "/usr/bin", "CHATKEEPER_LAUNCHER_PID": "1"], launcherPID: 777)
        XCTAssertEqual(env["CHATKEEPER_LAUNCHER"], "menubar")
        XCTAssertEqual(env["CHATKEEPER_LAUNCHER_PID"], "777")
        XCTAssertEqual(env["PATH"], "/usr/bin")
    }
}

final class DiagnosticsTests: XCTestCase {
    func testDiagnosticsRedactsAndBoundsLines() {
        let secret = "xoxb-" + "synthetic-credential"
        let lines = (0..<250).map { "line \($0)" }.joined(separator: "\n") + "\nBearer abc-secret\n\(secret)\ntoken=launch-secret"
        let text = diagnostics(version: "1.2.3", os: "testOS", log: lines)
        XCTAssertTrue(text.contains("1.2.3"))
        XCTAssertTrue(text.contains("testOS"))
        XCTAssertFalse(text.contains("synthetic-credential"))
        XCTAssertFalse(text.contains("abc-secret"))
        XCTAssertFalse(text.contains("launch-secret"))
        XCTAssertFalse(text.contains("line 0\n"))
        XCTAssertTrue(text.contains("line 249"))
    }
    func testRotation() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let url = dir.appendingPathComponent("child.log")
        try Data("first".utf8).write(to: url)
        try rotateLog(at: url, incoming: 20, maxBytes: 10, backups: 2)
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.path))
        XCTAssertEqual(try String(contentsOf: URL(fileURLWithPath: url.path + ".1")), "first")
    }
}

final class SplitCredentialTests: XCTestCase {
    func testRedactAfterLineAssembly() {
        var lines = LineBuffer()
        XCTAssertEqual(lines.append(Data("Bearer synthetic-".utf8)), [])
        let complete = lines.append(Data("secret\n".utf8)).map(redactSecrets).joined()
        XCTAssertFalse(complete.contains("synthetic-secret"))
        XCTAssertTrue(complete.contains("REDACTED"))
    }
}

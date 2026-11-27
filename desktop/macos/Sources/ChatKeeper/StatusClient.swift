import ChatKeeperKit
import Foundation

/// Talks to the local server: unauthenticated /healthz plus the
/// launch-token-authenticated /api/desktop endpoints.
struct StatusClient {
    let info: LaunchInfo

    private static let session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 3
        c.httpCookieAcceptPolicy = .never
        c.httpShouldSetCookies = false
        c.urlCache = nil
        return URLSession(configuration: c)
    }()

    func healthy() async -> Bool {
        do {
            let (_, response) = try await Self.session.data(from: info.endpoint("/healthz"))
            return (response as? HTTPURLResponse)?.statusCode == 200
        } catch {
            return false
        }
    }

    func status() async throws -> DesktopStatus {
        var request = URLRequest(url: info.endpoint("/api/desktop/status"))
        request.setValue("Bearer \(info.token)", forHTTPHeaderField: "Authorization")
        return try await send(request)
    }

    func setPaused(_ paused: Bool) async throws -> DesktopStatus {
        var request = URLRequest(url: info.endpoint("/api/desktop/pause"))
        request.httpMethod = "POST"
        request.setValue("Bearer \(info.token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["paused": paused])
        return try await send(request)
    }

    private func send(_ request: URLRequest) async throws -> DesktopStatus {
        let (data, response) = try await Self.session.data(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        return try DesktopStatus.decode(data)
    }
}

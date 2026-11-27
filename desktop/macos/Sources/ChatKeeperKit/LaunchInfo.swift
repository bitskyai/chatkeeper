import Foundation

/// The per-launch login URL printed by `chatkeeper run` in desktop mode
/// (`Open ChatKeeper: http://127.0.0.1:8787/login?token=…`) or stored in the
/// application home's `instance.url`. The token doubles as the bearer
/// credential for the local `/api/desktop/*` endpoints.
public struct LaunchInfo: Equatable, Sendable {
    public static let stdoutPrefix = "Open ChatKeeper: "

    public let launchURL: URL
    public let baseURL: URL
    public let token: String

    /// Accepts only loopback http URLs on /login with a non-empty token.
    public init?(url: URL) {
        guard url.scheme == "http",
              let host = url.host, LaunchInfo.isLoopback(host),
              url.path == "/login",
              url.user == nil, url.password == nil,
              let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let token = components.queryItems?.first(where: { $0.name == "token" })?.value,
              !token.isEmpty
        else { return nil }
        var base = URLComponents()
        base.scheme = "http"
        let bare = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        if bare.contains(":") {
            base.percentEncodedHost = "[\(bare)]"
        } else {
            base.host = bare
        }
        base.port = url.port
        guard let baseURL = base.url else { return nil }
        self.launchURL = url
        self.baseURL = baseURL
        self.token = token
    }

    public init?(string: String) {
        guard let url = URL(string: string.trimmingCharacters(in: .whitespacesAndNewlines)) else { return nil }
        self.init(url: url)
    }

    /// Parses one line of child stdout.
    public static func parse(line: String) -> LaunchInfo? {
        guard let range = line.range(of: stdoutPrefix) else { return nil }
        return LaunchInfo(string: String(line[range.upperBound...]))
    }

    /// Login URL that lands on `next` (a server-relative path) after sign-in.
    public func loginURL(next: String? = nil) -> URL {
        guard let next, next.hasPrefix("/"),
              var components = URLComponents(url: launchURL, resolvingAgainstBaseURL: false)
        else { return launchURL }
        var items = components.queryItems ?? []
        items.removeAll { $0.name == "next" }
        items.append(URLQueryItem(name: "next", value: next))
        components.queryItems = items
        // URLComponents leaves "/" and "?" unescaped in values; the server
        // decodes either form, but escape "&", "+" and "#" defensively.
        components.percentEncodedQuery = components.percentEncodedQuery?
            .replacingOccurrences(of: "+", with: "%2B")
        return components.url ?? launchURL
    }

    public func endpoint(_ path: String) -> URL {
        baseURL.appendingPathComponent(String(path.drop(while: { $0 == "/" })))
    }

    static func isLoopback(_ host: String) -> Bool {
        let h = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]")).lowercased()
        return h == "localhost" || h == "::1" || h.hasPrefix("127.")
    }
}

/// Removes per-launch tokens before child output is written to a log file.
public func redactSecrets(_ line: String) -> String {
    guard let regex = try? NSRegularExpression(pattern: "token=[^&\\s]+") else { return line }
    let range = NSRange(line.startIndex..., in: line)
    return regex.stringByReplacingMatches(in: line, range: range, withTemplate: "token=REDACTED")
}

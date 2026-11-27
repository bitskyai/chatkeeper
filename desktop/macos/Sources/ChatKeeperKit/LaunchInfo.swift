import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#endif

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

    /// `localhost`, or an IP literal in 127.0.0.0/8 or ::1. Hostnames that
    /// merely start with "127." (e.g. `127.0.0.1.evil.example`) are rejected.
    static func isLoopback(_ host: String) -> Bool {
        let h = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]")).lowercased()
        if h == "localhost" { return true }
        var v4 = in_addr()
        if inet_pton(AF_INET, h, &v4) == 1 {
            return UInt32(bigEndian: v4.s_addr) >> 24 == 127
        }
        var v6 = in6_addr()
        if inet_pton(AF_INET6, h, &v6) == 1 {
            let bytes = withUnsafeBytes(of: &v6) { Array($0) }
            return bytes == Array(repeating: 0, count: 15) + [1]
        }
        return false
    }
}

/// Removes per-launch tokens before child output is written to a log file.
public func redactSecrets(_ line: String) -> String {
    var result = line
    for pattern in ["token=[^&\\s]+", "\\bxox[a-z]?[.-][A-Za-z0-9._-]+", "\\bxapp-[A-Za-z0-9-]+", "(?i)bearer\\s+[^\\s\"',;]+", "(?i)(authorization|cookie|set-cookie)\\s*[:=]\\s*[^\\n]+", "[A-Za-z0-9+/_=-]{48,}"] {
        if let regex = try? NSRegularExpression(pattern: pattern) {
            result = regex.stringByReplacingMatches(in: result, range: NSRange(result.startIndex..., in: result), withTemplate: pattern.hasPrefix("token=") ? "token=REDACTED" : "REDACTED")
        }
    }
    return result
}

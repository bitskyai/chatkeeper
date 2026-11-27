import Foundation

/// Call while no log handle is open; the caller serializes writes.
public func rotateLog(at url: URL, incoming: Int, maxBytes: Int = 10 * 1024 * 1024, backups: Int = 5) throws {
    let fm = FileManager.default
    guard backups > 0, let info = try? fm.attributesOfItem(atPath: url.path),
          let size = info[.size] as? NSNumber, size.intValue > 0,
          size.intValue + incoming > maxBytes else { return }
    let oldest = url.path + ".\(backups)"
    if fm.fileExists(atPath: oldest) { try fm.removeItem(atPath: oldest) }
    for index in stride(from: backups - 1, through: 0, by: -1) {
        let source = index == 0 ? url.path : url.path + ".\(index)"
        if fm.fileExists(atPath: source) { try fm.moveItem(atPath: source, toPath: url.path + ".\(index + 1)") }
    }
}

public func diagnostics(version: String, os: String, log: String) -> String {
    let tail = log.split(separator: "\n", omittingEmptySubsequences: false).suffix(200).joined(separator: "\n")
    return "ChatKeeper \(version)\nOS: \(os)\n" + redactSecrets(tail)
}

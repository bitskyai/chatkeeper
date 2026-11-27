import AppKit
import Foundation
import Sparkle

/// Sparkle 2 wrapper. Updates are enabled only when the bundle carries both a
/// feed URL and an EdDSA public key (injected by build-app.sh from
/// SPARKLE_PUBLIC_ED_KEY). Without a key, "Check for Updates" opens the
/// GitHub Releases page instead of offering unverifiable updates.
final class Updater {
    static let releasesPage = URL(string: "https://github.com/bitskyai/chatkeeper/releases/latest")!

    private let controller: SPUStandardUpdaterController?

    init() {
        let info = Bundle.main.infoDictionary ?? [:]
        let key = (info["SUPublicEDKey"] as? String)?.trimmingCharacters(in: .whitespaces) ?? ""
        let feed = (info["SUFeedURL"] as? String) ?? ""
        if !key.isEmpty, !feed.isEmpty {
            controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
        } else {
            controller = nil
        }
    }

    var isConfigured: Bool { controller != nil }

    func checkForUpdates() {
        if let controller {
            NSApp.activate(ignoringOtherApps: true)
            controller.checkForUpdates(nil)
        } else {
            NSWorkspace.shared.open(Self.releasesPage)
        }
    }
}

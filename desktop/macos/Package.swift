// swift-tools-version:5.9
// ChatKeeper menu-bar app. Build with `swift build`; assemble the .app with
// scripts/build-app.sh. ChatKeeperKit is Foundation-only so its logic is unit
// tested by `swift test`; the ChatKeeper target needs macOS (AppKit, SwiftUI).
import PackageDescription

var targets: [Target] = [
    .target(name: "ChatKeeperKit"),
    .testTarget(name: "ChatKeeperKitTests", dependencies: ["ChatKeeperKit"]),
]
var dependencies: [Package.Dependency] = []

#if os(macOS)
// Pinned by commit (Sparkle 2.10.1); Package.resolved is committed and CI
// resolves with --force-resolved-versions. Bump both together.
dependencies.append(.package(url: "https://github.com/sparkle-project/Sparkle", revision: "ab7d71291238df6e0492143b120c9b696a35f603"))
targets.append(
    .executableTarget(
        name: "ChatKeeper",
        dependencies: ["ChatKeeperKit", .product(name: "Sparkle", package: "Sparkle")],
        linkerSettings: [
            // Sparkle.framework is embedded in Contents/Frameworks by build-app.sh.
            .unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"]),
        ]
    )
)
#endif

let package = Package(
    name: "ChatKeeperDesktop",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "ChatKeeperKit", targets: ["ChatKeeperKit"]),
    ],
    dependencies: dependencies,
    targets: targets
)

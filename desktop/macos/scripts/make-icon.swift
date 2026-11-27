// Renders the ChatKeeper app icon (1024×1024 PNG) with CoreGraphics so no
// binary artwork lives in the repo. Usage: swift make-icon.swift out.png
// Design: rounded teal→indigo tile, white chat bubble, teal bookmark ribbon.
import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

let size = 1024
let s = CGFloat(size)
guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write(Data("usage: make-icon.swift out.png\n".utf8))
    exit(2)
}
let space = CGColorSpace(name: CGColorSpace.sRGB)!
let ctx = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: 0, space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!

func color(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> CGColor {
    CGColor(colorSpace: space, components: [r / 255, g / 255, b / 255, a])!
}

// Tile: macOS icon grid leaves ~10% margin around a continuous rounded square.
let inset = s * 0.1
let tile = CGRect(x: inset, y: inset, width: s - 2 * inset, height: s - 2 * inset)
let tilePath = CGPath(roundedRect: tile, cornerWidth: tile.width * 0.225, cornerHeight: tile.width * 0.225, transform: nil)
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -s * 0.012), blur: s * 0.03, color: color(0, 0, 0, 0.35))
ctx.addPath(tilePath)
ctx.setFillColor(color(20, 110, 120))
ctx.fillPath()
ctx.restoreGState()
ctx.saveGState()
ctx.addPath(tilePath)
ctx.clip()
let gradient = CGGradient(colorsSpace: space, colors: [color(32, 178, 170), color(58, 72, 170)] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(gradient, start: CGPoint(x: tile.minX, y: tile.maxY), end: CGPoint(x: tile.maxX, y: tile.minY), options: [])
ctx.restoreGState()

// Chat bubble with a tail at the lower left.
let bubble = CGRect(x: s * 0.24, y: s * 0.33, width: s * 0.52, height: s * 0.40)
let bubblePath = CGMutablePath()
bubblePath.addRoundedRect(in: bubble, cornerWidth: s * 0.11, cornerHeight: s * 0.11)
bubblePath.move(to: CGPoint(x: s * 0.33, y: s * 0.35))
bubblePath.addLine(to: CGPoint(x: s * 0.27, y: s * 0.24))
bubblePath.addLine(to: CGPoint(x: s * 0.44, y: s * 0.34))
bubblePath.closeSubpath()
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -s * 0.008), blur: s * 0.02, color: color(0, 0, 0, 0.25))
ctx.addPath(bubblePath)
ctx.setFillColor(color(255, 255, 255))
ctx.fillPath()
ctx.restoreGState()

// Bookmark ribbon hanging from the bubble's top edge: "kept".
let rx = s * 0.555, rw = s * 0.11, top = bubble.maxY, bottom = s * 0.47
let ribbon = CGMutablePath()
ribbon.move(to: CGPoint(x: rx, y: top))
ribbon.addLine(to: CGPoint(x: rx + rw, y: top))
ribbon.addLine(to: CGPoint(x: rx + rw, y: bottom))
ribbon.addLine(to: CGPoint(x: rx + rw / 2, y: bottom + s * 0.05))
ribbon.addLine(to: CGPoint(x: rx, y: bottom))
ribbon.closeSubpath()
ctx.addPath(ribbon)
ctx.setFillColor(color(32, 160, 160))
ctx.fillPath()

// Three message lines.
ctx.setFillColor(color(58, 72, 170, 0.85))
for (i, w) in [0.20, 0.15, 0.21].enumerated() {
    let y = s * (0.62 - 0.075 * CGFloat(i))
    let line = CGRect(x: s * 0.31, y: y, width: s * CGFloat(w), height: s * 0.035)
    ctx.addPath(CGPath(roundedRect: line, cornerWidth: s * 0.0175, cornerHeight: s * 0.0175, transform: nil))
    ctx.fillPath()
}

let image = ctx.makeImage()!
let url = URL(fileURLWithPath: CommandLine.arguments[1])
let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil)!
CGImageDestinationAddImage(dest, image, nil)
guard CGImageDestinationFinalize(dest) else { exit(1) }

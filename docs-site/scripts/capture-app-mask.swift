// capture-app-mask.swift — replace the text of chosen rows in a menu capture.
//
//   swift capture-app-mask.swift <in.png> <out.png> <scale> "<yTop>,<height>,<text>" ...
//
// yTop and height are the row's rectangle in points relative to the top of
// the capture (as System Events reports them); scale is the backing scale
// (2 on Retina). macOS menus are translucent, so the row's background is a
// blurred gradient rather than a flat colour. For each row the script finds
// the bright text pixels, covers them with a copy of the text-free strip
// immediately to their right on the same row (so the gradient continues),
// and draws the replacement text there in the system menu font.
import AppKit

let args = CommandLine.arguments
guard args.count >= 5 else {
    FileHandle.standardError.write("usage: mask.swift in.png out.png scale spec...\n".data(using: .utf8)!)
    exit(2)
}
let inPath = args[1], outPath = args[2]
let scale = CGFloat(Double(args[3]) ?? 2)
guard let data = FileManager.default.contents(atPath: inPath),
      let src = NSBitmapImageRep(data: data) else {
    FileHandle.standardError.write("cannot read \(inPath)\n".data(using: .utf8)!)
    exit(1)
}
let W = src.pixelsWide, H = src.pixelsHigh

guard let out = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: W, pixelsHigh: H,
                                 bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                                 colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else { exit(1) }
NSGraphicsContext.saveGraphicsState()
let ctx = NSGraphicsContext(bitmapImageRep: out)!
NSGraphicsContext.current = ctx
let srcImage = NSImage(size: NSSize(width: W, height: H))
srcImage.addRepresentation(src)
srcImage.draw(in: NSRect(x: 0, y: 0, width: W, height: H), from: NSRect(x: 0, y: 0, width: W, height: H), operation: .copy, fraction: 1)

func lum(_ c: NSColor) -> CGFloat {
    let c = c.usingColorSpace(.deviceRGB)!
    return 0.2126 * c.redComponent + 0.7152 * c.greenComponent + 0.0722 * c.blueComponent
}

for spec in args[4...] {
    let parts = spec.split(separator: ",", maxSplits: 2, omittingEmptySubsequences: false).map(String.init)
    guard parts.count == 3, let yTop = Double(parts[0]), let hPt = Double(parts[1]) else { continue }
    let text = parts[2]
    let y0 = max(0, Int(yTop * Double(scale))), rh = Int(hPt * Double(scale))
    let y1 = min(H - 1, y0 + rh - 1)
    guard y1 > y0 else { continue }
    // Text pixels: bright, in the left three quarters of the row (the right
    // edge holds chevrons and the window's own rounded border).
    var bx0 = Int.max, bx1 = -1, by0 = Int.max, by1 = -1
    var brightest = NSColor.white, bl: CGFloat = 0
    for y in y0...y1 {
        for x in 0..<(W * 3 / 4) {
            let c = src.colorAt(x: x, y: y)!
            let l = lum(c)
            if l > 0.55 {
                bx0 = min(bx0, x); bx1 = max(bx1, x); by0 = min(by0, y); by1 = max(by1, y)
                if l > bl { bl = l; brightest = c }
            }
        }
    }
    if bx1 < 0 { print("row at \(yTop): nothing to mask"); continue }
    let pad = 4
    let cx0 = max(0, bx0 - pad), cx1 = min(W - 1, bx1 + pad)
    let cy0 = max(y0, by0 - pad), cy1 = min(y1, by1 + pad)
    let cw = cx1 - cx0 + 1, ch = cy1 - cy0 + 1
    // Cover the text with the strip just to its right on the same row.
    let sx0 = cx1 + 2
    let dest = NSRect(x: CGFloat(cx0), y: CGFloat(H - (cy1 + 1)), width: CGFloat(cw), height: CGFloat(ch))
    if sx0 + cw < W * 3 / 4 {
        let from = NSRect(x: CGFloat(sx0), y: CGFloat(H - (cy1 + 1)), width: CGFloat(cw), height: CGFloat(ch))
        srcImage.draw(in: dest, from: from, operation: .copy, fraction: 1)
    } else {
        src.colorAt(x: W - 40, y: (cy0 + cy1) / 2)!.setFill()
        dest.fill()
    }
    let font = NSFont.menuFont(ofSize: NSFont.systemFontSize * scale)
    let attrs: [NSAttributedString.Key: Any] = [.font: font, .foregroundColor: brightest]
    let str = NSAttributedString(string: text, attributes: attrs)
    let size = str.size()
    let cy = CGFloat(H) - CGFloat(by0 + by1 + 1) / 2   // centre of the old text, y-up
    str.draw(at: NSPoint(x: CGFloat(bx0), y: cy - size.height / 2))
    print("row at \(yTop): text box x \(bx0)-\(bx1) y \(by0)-\(by1) -> \(text)")
}
NSGraphicsContext.restoreGraphicsState()
guard let png = out.representation(using: .png, properties: [:]) else { exit(1) }
try! png.write(to: URL(fileURLWithPath: outPath))
print("wrote \(outPath) \(W)x\(H)")

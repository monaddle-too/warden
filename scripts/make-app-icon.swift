import AppKit
let output = URL(fileURLWithPath: CommandLine.arguments[1])
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
for points in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let size = points * scale
        let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
        NSGraphicsContext.saveGraphicsState(); NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
        let n = CGFloat(size)
        NSColor(calibratedRed: 0.13, green: 0.27, blue: 0.20, alpha: 1).setFill()
        NSBezierPath(roundedRect: NSRect(x: n*0.06, y: n*0.06, width: n*0.88, height: n*0.88), xRadius: n*0.2, yRadius: n*0.2).fill()
        NSColor(calibratedRed: 0.9, green: 0.95, blue: 0.87, alpha: 1).setFill()
        let shield = NSBezierPath(); shield.move(to: NSPoint(x:n*0.5,y:n*0.83)); shield.line(to:NSPoint(x:n*0.77,y:n*0.72)); shield.line(to:NSPoint(x:n*0.73,y:n*0.43)); shield.curve(to:NSPoint(x:n*0.5,y:n*0.19),controlPoint1:NSPoint(x:n*0.7,y:n*0.33),controlPoint2:NSPoint(x:n*0.57,y:n*0.22)); shield.curve(to:NSPoint(x:n*0.27,y:n*0.43),controlPoint1:NSPoint(x:n*0.43,y:n*0.22),controlPoint2:NSPoint(x:n*0.3,y:n*0.33)); shield.line(to:NSPoint(x:n*0.23,y:n*0.72)); shield.close(); shield.fill()
        let word = NSAttributedString(string:"W", attributes:[.font:NSFont.systemFont(ofSize:n*0.32,weight:.heavy),.foregroundColor:NSColor(calibratedRed:0.13,green:0.27,blue:0.20,alpha:1)])
        let textSize = word.size(); word.draw(at:NSPoint(x:(n-textSize.width)/2,y:n*0.39))
        NSGraphicsContext.restoreGraphicsState()
        let name = "icon_\(points)x\(points)\(scale == 2 ? "@2x" : "").png"
        try bitmap.representation(using:.png,properties:[:])!.write(to:output.appendingPathComponent(name))
    }
}

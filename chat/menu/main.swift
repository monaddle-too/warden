// warden-menu: Warden's item in the macOS menu bar (docs/menu-bar-plan.md).
//
// A bare AppKit executable, no bundle: it owns one NSStatusItem whose icon
// says whether Warden runs, whether an agent is working and whether
// something waits on the owner, and a dropdown that takes them there. It
// decides nothing itself: its model is the JSON lines `warden menu feed`
// writes, and every action runs `warden`. scripts/release.sh builds it
// with swiftc for the darwin tarball; `warden install` registers it as a
// launchd agent (cmd/warden/svc.go).
//
//   warden-menu --warden /path/to/warden [--config warden.json | --state DIR]

import AppKit
import Foundation

// MARK: - The model, as `warden menu feed` writes it (cmd/warden/menu.go)

struct Feed: Decodable {
    struct Attention: Decodable {
        let chatID: String
        let chat: String
        let kind: String // approval, review, error
        let label: String
    }
    struct Chat: Decodable {
        let id: String
        let title: String
        let status: String
        let activity: String?
        let lastActive: Double
    }
    struct Spend: Decodable {
        let todayUSD: Double
        let turns: Int
        let priced: Bool
    }
    let service: String // running, starting, stopped
    let registered: Bool
    let state: String
    let attention: [Attention]
    let chats: [Chat]
    let more: Int
    let working: Int
    let spend: Spend?
}

// MARK: - Options

struct Options {
    var warden = ""
    var passthrough: [String] = [] // --config / --state, handed to every warden command

    static func parse(_ args: [String]) -> Options {
        var o = Options()
        var i = 0
        while i < args.count {
            let a = args[i]
            switch a {
            case "--warden":
                i += 1
                if i < args.count { o.warden = args[i] }
            case "--config", "--state":
                i += 1
                if i < args.count { o.passthrough += [a, args[i]] }
            default:
                break
            }
            i += 1
        }
        if o.warden.isEmpty {
            // Beside this executable, as the tarball lays them out.
            let me = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath()
            o.warden = me.deletingLastPathComponent().appendingPathComponent("warden").path
        }
        return o
    }
}

// MARK: - The feed process

/// Feed runs `warden menu feed`, hands every line to `onLine` on the main
/// thread, and starts it again after it exits.
final class FeedProcess {
    let options: Options
    let onLine: (Feed) -> Void
    let onExit: () -> Void
    private var process: Process?
    private var buffer = Data()
    private let lock = NSLock()

    init(options: Options, onLine: @escaping (Feed) -> Void, onExit: @escaping () -> Void) {
        self.options = options
        self.onLine = onLine
        self.onExit = onExit
    }

    func start() {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: options.warden)
        p.arguments = ["menu", "feed"] + options.passthrough
        let out = Pipe()
        let input = Pipe() // held open: the feed ends when its stdin closes
        p.standardOutput = out
        p.standardInput = input
        p.standardError = FileHandle.standardError
        out.fileHandleForReading.readabilityHandler = { [weak self] handle in
            guard let self else { return }
            let data = handle.availableData
            if data.isEmpty { return }
            self.lock.lock()
            self.buffer.append(data)
            var lines: [Data] = []
            while let nl = self.buffer.firstIndex(of: UInt8(ascii: "\n")) {
                lines.append(self.buffer.subdata(in: self.buffer.startIndex..<nl))
                self.buffer.removeSubrange(self.buffer.startIndex...nl)
            }
            self.lock.unlock()
            for line in lines {
                guard let feed = try? JSONDecoder().decode(Feed.self, from: line) else { continue }
                DispatchQueue.main.async { self.onLine(feed) }
            }
        }
        p.terminationHandler = { [weak self] _ in
            out.fileHandleForReading.readabilityHandler = nil
            DispatchQueue.main.async {
                guard let self else { return }
                self.process = nil
                self.onExit()
                DispatchQueue.main.asyncAfter(deadline: .now() + 2) { self.start() }
            }
        }
        do {
            try p.run()
            process = p
        } catch {
            fputs("warden-menu: cannot run \(options.warden) menu feed: \(error)\n", stderr)
            DispatchQueue.main.asyncAfter(deadline: .now() + 5) { [weak self] in self?.start() }
        }
    }

    func stop() {
        process?.terminationHandler = nil
        process?.terminate()
        process = nil
    }
}

// MARK: - The status item

@MainActor
final class MenuBar: NSObject, NSMenuDelegate {
    let options: Options
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    var feed: FeedProcess!
    var model: Feed?
    var menuOpen = false
    var stale = false // the model changed while the menu was open

    init(options: Options) {
        self.options = options
        super.init()
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        item.button?.setAccessibilityLabel("Warden")
        feed = FeedProcess(options: options, onLine: { [weak self] m in
            self?.model = m
            self?.modelChanged()
        }, onExit: { [weak self] in
            self?.model = nil
            self?.modelChanged()
        })
        feed.start()
        renderIcon()
        rebuild()
    }

    func modelChanged() {
        renderIcon()
        if menuOpen {
            stale = true
        } else {
            rebuild()
        }
    }

    // The icon is the state: stopped, running, working, or attention with
    // the count beside it.
    func renderIcon() {
        guard let button = item.button else { return }
        var symbol = "shield.slash"
        var title = ""
        var tip = "Warden: unavailable"
        if let m = model {
            switch m.service {
            case "running":
                symbol = m.working > 0 ? "shield.lefthalf.filled" : "shield"
                tip = m.working > 0 ? "Warden: \(m.working) working" : "Warden: running"
            case "starting":
                symbol = "shield"
                tip = "Warden: starting"
            default:
                tip = "Warden: stopped"
            }
            if !m.attention.isEmpty {
                symbol = "exclamationmark.shield.fill"
                title = " \(m.attention.count)"
                tip = "Warden: \(m.attention.count) waiting on you"
            }
        }
        let image = NSImage(systemSymbolName: symbol, accessibilityDescription: tip)
        image?.isTemplate = true
        button.image = image
        button.imagePosition = title.isEmpty ? .imageOnly : .imageLeading
        button.title = title
        button.toolTip = tip
    }

    // MARK: NSMenuDelegate

    func menuWillOpen(_ menu: NSMenu) {
        menuOpen = true
        rebuild() // idle times are relative to now
    }

    func menuDidClose(_ menu: NSMenu) {
        menuOpen = false
        if stale {
            stale = false
            rebuild()
        }
    }

    // MARK: The dropdown

    func rebuild() {
        menu.removeAllItems()
        let m = model
        menu.addItem(header(m))
        if let m, !m.attention.isEmpty {
            menu.addItem(.separator())
            for a in m.attention.prefix(8) {
                let prefix: String
                switch a.kind {
                case "approval": prefix = "Approve"
                case "review": prefix = "Review"
                default: prefix = "Failed"
                }
                let row = action("\(prefix): \(clip(a.label, 60)) — \(clip(a.chat, 30))", #selector(openChat(_:)), symbol: a.kind == "error" ? "exclamationmark.triangle" : "hand.raised")
                row.representedObject = a.chatID
            }
            if m.attention.count > 8 {
                action("\(m.attention.count - 8) more…", #selector(openApp), symbol: nil)
            }
        }
        if let m, !m.chats.isEmpty {
            menu.addItem(.separator())
            for c in m.chats {
                let row = action(chatLine(c), #selector(openChat(_:)), symbol: nil)
                row.representedObject = c.id
            }
            if m.more > 0 {
                action("\(m.more) more…", #selector(openApp), symbol: nil)
            }
        }
        menu.addItem(.separator())
        let running = m?.service == "running"
        action("New Chat…", #selector(newChat), symbol: "plus.bubble", key: "n").isEnabled = running
        action("Open Warden", #selector(openApp), symbol: "macwindow", key: "o").isEnabled = running
        if let s = m?.spend {
            let cost = s.priced ? String(format: "$%.2f", s.todayUSD) : "unpriced"
            let row = NSMenuItem(title: "Today: \(cost) · \(s.turns) turn\(s.turns == 1 ? "" : "s")", action: nil, keyEquivalent: "")
            row.isEnabled = false
            menu.addItem(row)
        }
        menu.addItem(.separator())
        switch m?.service {
        case "running", "starting":
            action("Stop Warden", #selector(stopWarden), symbol: "stop")
            action("Restart Warden", #selector(restartWarden), symbol: "arrow.clockwise").isEnabled = running
        default:
            action("Start Warden", #selector(startWarden), symbol: "play").isEnabled = m != nil
        }
        action("Show Logs in Finder", #selector(showLogs), symbol: "folder").isEnabled = m != nil
        menu.addItem(.separator())
        action("Quit Menu Bar Item", #selector(quit), symbol: nil, key: "q")
    }

    func header(_ m: Feed?) -> NSMenuItem {
        var text = "Warden · unavailable"
        if let m {
            switch m.service {
            case "running":
                text = "Warden · running"
                if m.working > 0 { text += " · \(m.working) working" }
            case "starting":
                text = "Warden · starting…"
            default:
                text = m.registered ? "Warden · stopped" : "Warden · stopped (no service registered)"
            }
        }
        let row = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        row.attributedTitle = NSAttributedString(string: text, attributes: [.font: NSFont.boldSystemFont(ofSize: NSFont.systemFontSize)])
        row.isEnabled = false
        return row
    }

    func chatLine(_ c: Feed.Chat) -> String {
        let title = clip(c.title, 40)
        if let a = c.activity, !a.isEmpty {
            return "● \(title) — \(clip(a, 50))"
        }
        var state: String
        switch c.status {
        case "failed": state = "failed"
        case "interrupted": state = "interrupted"
        default: state = "idle"
        }
        if c.lastActive > 0 {
            state += " " + ago(Date().timeIntervalSince1970 - c.lastActive)
        }
        return "○ \(title) — \(state)"
    }

    func ago(_ seconds: Double) -> String {
        let s = max(0, seconds)
        if s < 60 { return "just now" }
        if s < 3600 { return "\(Int(s / 60)) min" }
        if s < 86400 { return "\(Int(s / 3600)) h" }
        return "\(Int(s / 86400)) d"
    }

    func clip(_ s: String, _ n: Int) -> String {
        let one = s.replacingOccurrences(of: "\n", with: " ")
        if one.count <= n { return one }
        return String(one.prefix(n - 1)) + "…"
    }

    @discardableResult
    func action(_ title: String, _ selector: Selector, symbol: String?, key: String = "") -> NSMenuItem {
        let row = NSMenuItem(title: title, action: selector, keyEquivalent: key)
        row.target = self
        if let symbol {
            row.image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)
        }
        menu.addItem(row)
        return row
    }

    // MARK: Actions: every one runs `warden`

    @objc func openApp() { warden(["open"]) }
    @objc func newChat() { warden(["open", "--new"]) }
    @objc func openChat(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? String else { return }
        warden(["open", "--chat", id])
    }
    @objc func startWarden() {
        // Without a registered service `warden start` runs the stack in
        // the foreground, which is not what a menu click means.
        warden(model?.registered == true ? ["start"] : ["start", "--detach"])
    }
    @objc func stopWarden() { warden(["stop"]) }
    @objc func restartWarden() { warden(["restart"]) }
    @objc func showLogs() {
        guard let state = model?.state else { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: state, isDirectory: true))
    }
    @objc func quit() {
        feed.stop()
        NSApp.terminate(nil)
    }

    /// warden runs a command in the background; a failure is shown once
    /// it exits, with the command's output.
    func warden(_ args: [String]) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: options.warden)
        p.arguments = args + options.passthrough
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        p.standardInput = FileHandle.nullDevice
        p.terminationHandler = { proc in
            let data = out.fileHandleForReading.readDataToEndOfFile()
            guard proc.terminationStatus != 0 else { return }
            let text = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            DispatchQueue.main.async {
                let alert = NSAlert()
                alert.messageText = "warden \(args.joined(separator: " ")) failed"
                alert.informativeText = text.isEmpty ? "exit status \(proc.terminationStatus)" : text
                alert.alertStyle = .warning
                NSApp.activate(ignoringOtherApps: true)
                alert.runModal()
            }
        }
        do {
            try p.run()
        } catch {
            let alert = NSAlert()
            alert.messageText = "Cannot run warden"
            alert.informativeText = "\(options.warden): \(error.localizedDescription)"
            NSApp.activate(ignoringOtherApps: true)
            alert.runModal()
        }
    }
}

// MARK: - main

let options = Options.parse(Array(CommandLine.arguments.dropFirst()))
let app = NSApplication.shared
app.setActivationPolicy(.accessory) // no Dock icon, no menu of its own
let bar = MainActor.assumeIsolated { MenuBar(options: options) }
// SIGTERM (launchctl bootout, `warden uninstall`) ends the feed with us.
signal(SIGTERM) { _ in
    DispatchQueue.main.async { MainActor.assumeIsolated { bar.quit() } }
}
app.run()

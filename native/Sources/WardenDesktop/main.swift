import AppKit

final class DesktopApp: NSObject, NSApplicationDelegate, NSWindowDelegate {
    var window: NSWindow!
    var logWindow: NSWindow?
    let heading = NSTextField(wrappingLabelWithString: "Your space to build.")
    let subtitle = NSTextField(wrappingLabelWithString: "A separate Mac for your projects and AI tools.\nWarden takes care of the setup.")
    let summary = NSTextField(wrappingLabelWithString: "macOS + developer tools\n16 GiB memory · 70 GiB free space recommended")
    let note = NSTextField(wrappingLabelWithString: "Setup uses local installers. No downloads.")
    let stepLabel = NSTextField(wrappingLabelWithString: "")
    let primary = NSButton(title: "Set up Warden", target: nil, action: nil)
    let customize = NSButton(title: "Customize…", target: nil, action: nil)
    let details = NSButton(title: "Details", target: nil, action: nil)
    let progress = NSProgressIndicator()
    let log = NSTextView()
    let preferences = UserDefaults(suiteName: ProcessInfo.processInfo.environment["WARDEN_SETUP_PREFERENCES"] ?? "dev.warden.desktop.setup")!
    var statePath = ""
    var inputsPath: String?
    var prepareOnly = false
    var snapshot: [String: Any] = [:]
    var busy = false
    var checking = false
    var failed = false
    var current: Process?
    var timer: Timer?
    var lineBuffer = ""
    var lastStep = ""
    var draftState = ""
    var draftInputs: String?
    var optionsAlert: NSAlert?
    let storageLabel = NSTextField(wrappingLabelWithString: "")
    let inputsLabel = NSTextField(wrappingLabelWithString: "")
    let installToggle = NSButton(checkboxWithTitle: "Install macOS automatically", target: nil, action: nil)
    var launcher: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/warden") }
    var environment: [String: String] {
        var value = ProcessInfo.processInfo.environment; value["WARDEN_STATE"] = statePath; return value
    }
    func yes(_ key: String) -> Bool { snapshot[key] as? Bool == true }
    func label(_ text: String, size: CGFloat = 13) -> NSTextField {
        let view = NSTextField(wrappingLabelWithString: text); view.font = .systemFont(ofSize: size); return view
    }
    func applicationDidFinishLaunching(_ notification: Notification) {
        statePath = ProcessInfo.processInfo.environment["WARDEN_STATE"] ?? preferences.string(forKey: "storage") ?? NSHomeDirectory()+"/Library/Application Support/Warden"
        inputsPath = preferences.string(forKey: "inputs"); prepareOnly = preferences.bool(forKey: "prepareOnly")
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 560, height: 500), styleMask: [.titled, .closable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "Warden"; window.titlebarAppearsTransparent = true; window.delegate = self; window.isReleasedWhenClosed = false
        let stack = NSStackView(); stack.orientation = .vertical; stack.alignment = .centerX; stack.spacing = 18
        stack.translatesAutoresizingMaskIntoConstraints = false; window.contentView!.addSubview(stack)
        NSLayoutConstraint.activate([stack.leadingAnchor.constraint(equalTo: window.contentView!.leadingAnchor, constant: 42), stack.trailingAnchor.constraint(equalTo: window.contentView!.trailingAnchor, constant: -42), stack.topAnchor.constraint(equalTo: window.contentView!.topAnchor, constant: 26)])
        let icon = NSImageView(); icon.image = NSImage(contentsOf: Bundle.main.resourceURL!.appendingPathComponent("Warden.icns")); icon.imageScaling = .scaleProportionallyUpOrDown
        icon.widthAnchor.constraint(equalToConstant: 72).isActive = true; icon.heightAnchor.constraint(equalToConstant: 72).isActive = true; stack.addArrangedSubview(icon)
        heading.font = .systemFont(ofSize: 28, weight: .semibold); heading.alignment = .center; stack.addArrangedSubview(heading)
        subtitle.font = .systemFont(ofSize: 14); subtitle.textColor = .secondaryLabelColor; subtitle.alignment = .center; stack.addArrangedSubview(subtitle)
        summary.font = .systemFont(ofSize: 12); summary.textColor = .secondaryLabelColor; summary.alignment = .center; stack.addArrangedSubview(summary)
        progress.style = .bar; progress.isIndeterminate = true; progress.isHidden = true; stack.addArrangedSubview(progress)
        stepLabel.font = .systemFont(ofSize: 12, weight: .medium); stepLabel.alignment = .center; stepLabel.isHidden = true; stack.addArrangedSubview(stepLabel)
        primary.target = self; primary.action = #selector(continueSetup); primary.bezelStyle = .rounded; primary.controlSize = .large; primary.keyEquivalent = "\r"
        primary.widthAnchor.constraint(equalToConstant: 240).isActive = true; primary.heightAnchor.constraint(equalToConstant: 40).isActive = true; stack.addArrangedSubview(primary)
        customize.target = self; customize.action = #selector(showCustomize); customize.bezelStyle = .inline; stack.addArrangedSubview(customize)
        note.font = .systemFont(ofSize: 11); note.textColor = .tertiaryLabelColor; note.alignment = .center; stack.addArrangedSubview(note)
        let footer = NSStackView(); footer.orientation = .horizontal; footer.spacing = 16
        details.target = self; details.action = #selector(showDetails); details.bezelStyle = .inline; details.font = .systemFont(ofSize: 11)
        let help = NSButton(title: "Help", target: self, action: #selector(guide)); help.bezelStyle = .inline; help.font = .systemFont(ofSize: 11); footer.addArrangedSubview(details); footer.addArrangedSubview(help); stack.addArrangedSubview(footer)
        for view in [heading, subtitle, summary, note, progress, stepLabel] { view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true }
        log.isEditable = false; log.isSelectable = true; log.font = .monospacedSystemFont(ofSize: 11, weight: .regular); log.autoresizingMask = [.width]; log.textContainer?.widthTracksTextView = true
        let appMenu = NSMenu(); let item = NSMenuItem(); appMenu.addItem(item); let submenu = NSMenu(); item.submenu = submenu
        submenu.addItem(withTitle: "Quit Warden", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q"); NSApp.mainMenu = appMenu
        window.center(); window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
        primary.isEnabled = false; refresh(); timer = Timer.scheduledTimer(withTimeInterval: 10, repeats: true) { [weak self] _ in self?.refresh() }
    }
    func render() {
        guard !busy else { return }
        progress.isHidden = true; stepLabel.isHidden = true; summary.isHidden = false
        customize.isEnabled = !yes("state_busy"); primary.isEnabled = !snapshot.isEmpty && !yes("state_busy")
        note.stringValue = "Setup uses local installers. No downloads."
        summary.stringValue = "macOS + developer tools\n16 GiB memory · At least 70 GiB free storage"
        if failed {
            heading.stringValue = "Let’s try that again."
            subtitle.stringValue = "Setup paused before finishing. Your progress is saved.\nYou can retry or check the details."
            primary.title = "Try again"; return
        }
        if yes("vm_running") {
            heading.stringValue = yes("paired") ? "You’re ready to build." : "Your Mac is ready."
            subtitle.stringValue = yes("paired") ? "Open Warden to manage your environment\nand review requests from your tools." : "Finish the welcome steps inside your new Mac.\nWe’ll help you connect it to Warden."
            primary.title = yes("paired") ? "Open Warden" : "Finish guest setup"
            primary.isEnabled = true; note.stringValue = "Your environment is running."
        } else if yes("mac_installed") && yes("tools_staged") {
            heading.stringValue = "Ready when you are."
            subtitle.stringValue = yes("controller_conflict") ? "Another Warden environment is open.\nClose it before starting this one." : "Your development Mac is set up.\nOpen it to finish your account and sign in to your tools."
            primary.title = "Open Warden"; primary.isEnabled = !yes("controller_conflict") && !yes("state_busy")
            note.stringValue = "The first launch needs internet to finish setting up the proxy."
        } else if yes("proxy_prepared") && prepareOnly {
            heading.stringValue = "Your images are ready."
            subtitle.stringValue = "Local preparation is complete.\nInstall macOS whenever you’re ready."
            primary.title = "Install macOS"; summary.stringValue = "Your files stay in the location you chose."
        } else {
            heading.stringValue = yes("proxy_prepared") || yes("mac_installed") ? "Pick up where you left off." : "Your space to build."
            subtitle.stringValue = "A separate Mac for your projects and AI tools.\nWarden takes care of the setup."
            primary.title = yes("proxy_prepared") || yes("mac_installed") ? "Continue setup" : "Set up Warden"
            if prepareOnly { summary.stringValue = "Prepare local images only\nYou can install macOS later." }
        }
        if yes("state_busy") && !yes("vm_running") { subtitle.stringValue = "Another setup step is still running.\nWe’ll be ready when it finishes." }
    }
    func append(_ text: String) {
        log.textStorage?.append(NSAttributedString(string: text)); if log.string.count > 60000 { log.string = String(log.string.suffix(50000)) }; log.scrollToEndOfDocument(nil)
        lineBuffer += text
        while let range = lineBuffer.range(of: "\n") {
            let line = String(lineBuffer[..<range.lowerBound]); lineBuffer.removeSubrange(..<range.upperBound)
            if let data = line.data(using: .utf8), let value = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
                if let step = value["setup_step"] as? String { updateStep(step) }
                if let fraction = value["installation_progress"] as? Double, fraction.isFinite {
                    stepLabel.stringValue = "Installing macOS · \(Int(max(0, min(1, fraction))*100))%"
                }
            }
        }
        if lineBuffer.count > 8192 { lineBuffer = String(lineBuffer.suffix(8192)) }
    }
    func updateStep(_ step: String) {
        lastStep = step
        let titles = ["importing":"Getting your installers ready…", "checking":"Checking your installers…", "preparing":"Preparing your development space…", "installing":"Installing macOS…", "tools":"Adding your developer tools…", "prepared":"Local images are ready.", "ready":"Your development Mac is ready."]
        stepLabel.stringValue = titles[step] ?? "Setting up Warden…"
    }
    func run(_ arguments: [String], completion: (() -> Void)? = nil) {
        guard !busy else { return }; busy = true; failed = false; lastStep = ""; lineBuffer = ""
        primary.isEnabled = false; primary.title = "Setting up…"; customize.isEnabled = false
        heading.stringValue = "Setting up Warden."
        subtitle.stringValue = "Warden is handling the setup.\nYou can leave this window open while it works."
        summary.isHidden = true; progress.isHidden = false; stepLabel.isHidden = false; stepLabel.stringValue = "Getting started…"; progress.startAnimation(nil)
        append("\n▶ " + arguments.joined(separator: " ") + "\n")
        let process = Process(); process.executableURL = launcher; process.arguments = arguments; process.environment = environment
        let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe; process.standardInput = FileHandle.nullDevice; current = process
        // One background reader drains all output before reporting completion.
        DispatchQueue.global().async {
            var success = false
            do {
                try process.run()
                while true {
                    let data = pipe.fileHandleForReading.availableData; if data.isEmpty { break }
                    let text = String(decoding: data, as: UTF8.self); DispatchQueue.main.async { self.append(text) }
                }
                process.waitUntilExit(); success = process.terminationStatus == 0
            } catch { let message = error.localizedDescription; DispatchQueue.main.async { self.append(message+"\n") } }
            let succeeded = success
            DispatchQueue.main.async {
                self.current = nil; self.busy = false; self.failed = !succeeded; self.progress.stopAnimation(nil)
                self.append(succeeded ? "Completed.\n" : "Setup paused. See the output above.\n")
                if succeeded { completion?() }; self.render(); self.refresh()
            }
        }
    }
    @objc func refresh() {
        guard !busy, !checking else { return }; checking = true
        let process = Process(); process.executableURL = launcher; process.arguments = ["desktop", "status"]; process.environment = environment
        let requestedState = statePath
        let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe; process.standardInput = FileHandle.nullDevice
        DispatchQueue.global().async {
            var result: [String: Any]?; var errorText = ""
            do { try process.run(); let data = pipe.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit(); result = try? JSONSerialization.jsonObject(with: data) as? [String: Any]; if result == nil { errorText = String(decoding: data, as: UTF8.self) } }
            catch { errorText = error.localizedDescription }
            let value = result; let failure = errorText
            DispatchQueue.main.async {
                self.checking = false; guard !self.busy, self.statePath == requestedState else { return }
                if let value { self.snapshot = value; self.render() }
                else { self.snapshot = [:]; self.heading.stringValue = "Let’s get setup working."; self.subtitle.stringValue = "Warden couldn’t check this environment.\nChoose another location in Customize, or check the details."; self.primary.title = "Check again"; self.primary.isEnabled = true; self.append(failure+"\n") }
            }
        }
    }
    @objc func continueSetup() {
        if snapshot.isEmpty { refresh(); return }
        if yes("vm_running") && !yes("paired") { showPairing(); return }
        if yes("vm_running") && yes("paired") { run(["desktop", "dashboard"]); return }
        if yes("mac_installed") && yes("tools_staged") { run(["desktop", "start"]); return }
        let installNow = prepareOnly && yes("proxy_prepared")
        func begin(_ source: String?) {
            var arguments = ["desktop", "setup"]
            if let source { arguments.append(source) }
            if prepareOnly && !installNow { arguments.append("--prepare-only") }
            run(arguments)
        }
        if yes("inputs_ready") { begin(nil) }
        else if let source = inputsPath ?? snapshot["suggested_inputs"] as? String { begin(source) }
        else {
            chooseFolder(message: "Choose your local Warden installers. We’ll verify the files, prepare your space and continue setup.", prompt: "Use These Installers", parent: window) { source in
                self.inputsPath = source; self.preferences.set(source, forKey: "inputs"); begin(source)
            }
        }
    }
    func chooseFolder(message: String, prompt: String, parent: NSWindow, completion: @escaping (String) -> Void) {
        let panel = NSOpenPanel(); panel.canChooseFiles = false; panel.canChooseDirectories = true; panel.canCreateDirectories = true
        panel.prompt = prompt; panel.message = message
        panel.beginSheetModal(for: parent) { response in if response == .OK, let url = panel.url { completion(url.path) } }
    }
    @objc func showCustomize() {
        guard !busy else { return }; draftState = statePath; draftInputs = inputsPath ?? snapshot["suggested_inputs"] as? String
        let alert = NSAlert(); alert.messageText = "Make it yours."; alert.informativeText = "The recommended settings work for most projects."; alert.addButton(withTitle: "Done"); alert.addButton(withTitle: "Cancel")
        optionsAlert = alert
        let stack = NSStackView(); stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 12
        let sourceTitle = label("Installer files", size: 13); sourceTitle.font = .systemFont(ofSize: 13, weight: .semibold); stack.addArrangedSubview(sourceTitle)
        inputsLabel.stringValue = draftInputs.map { URL(fileURLWithPath: $0).lastPathComponent } ?? "Choose when setup starts"; inputsLabel.toolTip = draftInputs
        let cacheButton = NSButton(title: "Choose…", target: self, action: #selector(changeInputs)); let cacheRow = NSStackView(views: [inputsLabel, cacheButton]); cacheRow.spacing = 12; stack.addArrangedSubview(cacheRow)
        let storageTitle = label("Keep my environment in", size: 13); storageTitle.font = .systemFont(ofSize: 13, weight: .semibold); stack.addArrangedSubview(storageTitle)
        storageLabel.stringValue = shortPath(draftState); storageLabel.toolTip = draftState
        let storageButton = NSButton(title: "Change…", target: self, action: #selector(changeStorage)); storageButton.isEnabled = !yes("proxy_prepared") && !yes("mac_installed") && !yes("state_busy")
        let storageRow = NSStackView(views: [storageLabel, storageButton]); storageRow.spacing = 12; stack.addArrangedSubview(storageRow)
        let storageNote = label(storageButton.isEnabled ? "VM files stay here when you update the app." : "This environment already has files here. Its location stays fixed.", size: 11); storageNote.textColor = .secondaryLabelColor; stack.addArrangedSubview(storageNote)
        installToggle.state = prepareOnly ? .off : .on; stack.addArrangedSubview(installToggle)
        let installNote = label("Turn this off to prepare the images now and install macOS later.", size: 11); installNote.textColor = .secondaryLabelColor; stack.addArrangedSubview(installNote)
        stack.frame = NSRect(x: 0, y: 0, width: 420, height: 230)
        for view in [sourceTitle, cacheRow, storageTitle, storageRow, storageNote, installNote] { view.widthAnchor.constraint(equalToConstant: 420).isActive = true }
        alert.accessoryView = stack
        alert.beginSheetModal(for: window) { response in
            if response == .alertFirstButtonReturn {
                self.inputsPath = self.draftInputs; self.statePath = self.draftState; self.prepareOnly = self.installToggle.state == .off
                self.preferences.set(self.inputsPath, forKey: "inputs"); self.preferences.set(self.statePath, forKey: "storage"); self.preferences.set(self.prepareOnly, forKey: "prepareOnly")
                self.failed = false; self.snapshot = [:]; self.primary.isEnabled = false; self.refresh()
            }
            self.optionsAlert = nil
        }
    }
    func shortPath(_ path: String) -> String { path.hasPrefix(NSHomeDirectory()+"/") ? "~/"+path.dropFirst(NSHomeDirectory().count+1) : path }
    @objc func changeInputs() {
        guard let alert = optionsAlert else { return }
        chooseFolder(message: "Choose the folder containing your local installers.", prompt: "Choose", parent: alert.window) { path in self.draftInputs = path; self.inputsLabel.stringValue = URL(fileURLWithPath: path).lastPathComponent; self.inputsLabel.toolTip = path }
    }
    @objc func changeStorage() {
        guard let alert = optionsAlert else { return }
        chooseFolder(message: "Choose an empty folder for this environment. Existing VM files won’t be moved.", prompt: "Use This Folder", parent: alert.window) { path in
            let files = (try? FileManager.default.contentsOfDirectory(atPath: path)) ?? ["unreadable"]
            guard files.filter({ $0 != ".DS_Store" }).isEmpty else { let error = NSAlert(); error.messageText = "Choose an empty folder."; error.informativeText = "Your existing files will stay where they are."; error.beginSheetModal(for: alert.window); return }
            self.draftState = path; self.storageLabel.stringValue = self.shortPath(path); self.storageLabel.toolTip = path
        }
    }
    @objc func showDetails() {
        if logWindow == nil {
            let detailWindow = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 720, height: 430), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
            detailWindow.title = "Warden — Setup Details"; detailWindow.isReleasedWhenClosed = false
            let scroll = NSScrollView(frame: detailWindow.contentView!.bounds); scroll.autoresizingMask = [.width, .height]; scroll.hasVerticalScroller = true; scroll.documentView = log; detailWindow.contentView!.addSubview(scroll); detailWindow.center(); logWindow = detailWindow
        }
        if log.string.isEmpty { append("Environment: "+statePath+"\nWaiting for setup to start.\n") }
        logWindow?.makeKeyAndOrderFront(nil)
    }
    func showPairing() {
        let alert = NSAlert(); alert.messageText = "Connect your new Mac."; alert.informativeText = "Complete the welcome screens in the VM. Then open its Terminal and run sudo warden-trust-proxy. Enter the username and fingerprint printed by the installer."; alert.addButton(withTitle: "Connect"); alert.addButton(withTitle: "Later")
        let user = NSTextField(); user.placeholderString = "Guest username"; user.setAccessibilityLabel("Guest username")
        let fingerprint = NSTextField(); fingerprint.placeholderString = "SHA256:guest fingerprint"; fingerprint.setAccessibilityLabel("Guest fingerprint")
        let fields = NSStackView(views: [user, fingerprint]); fields.orientation = .vertical; fields.spacing = 12; fields.frame = NSRect(x: 0, y: 0, width: 420, height: 70)
        for field in [user, fingerprint] { field.widthAnchor.constraint(equalToConstant: 420).isActive = true }; alert.accessoryView = fields
        alert.beginSheetModal(for: window) { response in if response == .alertFirstButtonReturn { self.run(["guest", "pair", "--user", user.stringValue.trimmingCharacters(in: .whitespaces), "--fingerprint", fingerprint.stringValue.trimmingCharacters(in: .whitespaces)]) } }
    }
    @objc func guide() { NSWorkspace.shared.open(Bundle.main.resourceURL!.appendingPathComponent("Local Setup.html")) }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if busy { let alert = NSAlert(); alert.messageText = "Setup is still running."; alert.informativeText = "Let this step finish before quitting. You can hide the window while you wait."; alert.runModal(); return .terminateCancel }; return .terminateNow
    }
    func windowShouldClose(_ sender: NSWindow) -> Bool { sender.orderOut(nil); return false }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool { window.makeKeyAndOrderFront(nil); return true }
}
let application = NSApplication.shared
application.setActivationPolicy(.regular)
let delegate = DesktopApp(); application.delegate = delegate; application.run()

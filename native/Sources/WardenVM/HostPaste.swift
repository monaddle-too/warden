import AppKit
import Virtualization

// Only this host UI action reads NSPasteboard. No guest message can invoke it.
final class PasteTransport: NSObject, URLSessionTaskDelegate {
    let state: URL
    lazy var session: URLSession = {
        let config = URLSessionConfiguration.ephemeral
        config.urlCache = nil; config.httpCookieStorage = nil; config.urlCredentialStorage = nil
        config.connectionProxyDictionary = [:]
        config.timeoutIntervalForRequest = 3; config.timeoutIntervalForResource = 4
        return URLSession(configuration: config, delegate: self, delegateQueue: nil)
    }()
    init(state: URL) { self.state = state }
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
    func call(_ path: String, _ body: [String: Any]) async throws -> [String: Any] {
        let token = try String(contentsOf: state.appendingPathComponent("admin-token"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        var request = URLRequest(url: URL(string: "http://127.0.0.1:18765/api/clipboard" + path)!)
        request.httpMethod = "POST"
        request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization")
        request.setValue("http://127.0.0.1:18765", forHTTPHeaderField: "Origin")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, response) = try await session.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200, data.count < 4096,
              let result = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw PasteError.transfer }
        return result
    }
}

enum PasteError: Error { case transfer, timeout }

// Keep physical input state from the app's event stream. The global modifier
// snapshot also includes synthesized input and need not describe the shortcut
// whose release we are waiting for.
struct PasteInputGate {
    enum Action: Equatable { case forward, start, consume, cancelAndForward }
    private(set) var modifiers: NSEvent.ModifierFlags = []
    private(set) var shortcutDown = false
    var released: Bool { modifiers.isEmpty && !shortcutDown }

    mutating func handle(_ event: NSEvent, inGuest: Bool) -> Action {
        if [.keyDown, .keyUp, .flagsChanged].contains(event.type) {
            modifiers = event.modifierFlags.intersection([.command, .shift, .control, .option])
        }
        if event.type == .keyUp, event.keyCode == 9, shortcutDown {
            shortcutDown = false
            return .consume
        }
        if event.type == .keyDown, event.keyCode == 9, inGuest,
           modifiers == [.command, .shift] {
            let start = !shortcutDown && !event.isARepeat
            shortcutDown = true
            return start ? .start : .consume
        }
        if inGuest && [.keyDown, .leftMouseDown, .rightMouseDown, .otherMouseDown].contains(event.type) {
            return .cancelAndForward
        }
        return .forward
    }
    mutating func reset() { modifiers = []; shortcutDown = false }
}

// Virtualization installs local event monitors. Both shortcut interception and
// cancellation must run BEFORE those monitors, not in NSWindow.sendEvent.
@objc(WardenApplication)
final class WardenApplication: NSApplication {
    var pasteInput = PasteInputGate()
    override func sendEvent(_ event: NSEvent) {
        let window = keyWindow as? GuestWindow
        switch pasteInput.handle(event, inGuest: window != nil) {
        case .start:
            window?.hostPaste?.pasteFromHost(nil)
            return
        case .consume:
            return
        case .cancelAndForward:
            // A manual Cmd-V must cancel the scheduled Cmd-V before the VM sees it.
            window?.hostPaste?.cancel()
        case .forward:
            break
        }
        super.sendEvent(event)
    }
}

final class GuestWindow: NSWindow {
    weak var hostPaste: HostPaste?
}

@MainActor
final class HostPaste: NSObject, NSMenuItemValidation {
    weak var view: VZVirtualMachineView?
    weak var window: NSWindow?
    let transport: PasteTransport
    var pending: UUID?
    var task: Task<Void, Never>?
    var observer: NSObjectProtocol?
    init(view: VZVirtualMachineView, window: NSWindow, state: URL) {
        self.view = view; self.window = window; self.transport = PasteTransport(state: state)
        super.init()
        observer = NotificationCenter.default.addObserver(forName: NSWindow.didResignKeyNotification, object: window, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.cancel()
                (NSApp as? WardenApplication)?.pasteInput.reset()
            }
        }
    }
    var ready: Bool { window?.isKeyWindow == true && view?.virtualMachine?.state == .running && window?.attachedSheet == nil }
    func validateMenuItem(_ menuItem: NSMenuItem) -> Bool { ready && pending == nil }
    func cancel() {
        guard let id = pending else { return }
        pending = nil; task?.cancel(); task = nil
        window?.subtitle = "Paste cancelled"
        Task { _ = try? await transport.call("/cancel", ["id": id.uuidString.lowercased()]) }
    }
    @objc func pasteFromHost(_ sender: Any?) {
        guard ready, pending == nil, let view, let window else { return }
        guard let text = NSPasteboard.general.string(forType: .string) else {
            window.subtitle = "Host clipboard contains no text"; NSSound.beep(); return
        }
        guard text.utf8.count <= 65536 else {
            window.subtitle = "Paste from Host supports up to 64 KiB of text"; NSSound.beep(); return
        }
        let id = UUID(); let transferID = id.uuidString.lowercased()
        pending = id; window.subtitle = "Pasting from host…"
        window.makeFirstResponder(view)
        task = Task { [weak self] in
            guard let self else { return }
            do {
                let sent = try await transport.call("", ["text": text, "id": transferID])
                guard sent["id"] as? String == transferID else { throw PasteError.transfer }
                let deadline = ProcessInfo.processInfo.systemUptime + 10
                while pending == id && ready && window.firstResponder === view {
                    try Task.checkCancellation()
                    guard ProcessInfo.processInfo.systemUptime < deadline else { throw PasteError.timeout }
                    let result = try await transport.call("/status", ["id": transferID])
                    guard result["id"] as? String == transferID else { throw PasteError.transfer }
                    if result["status"] as? String == "delivered" {
                        // Wait until the physical shortcut modifiers have been
                        // released, so this cannot become Cmd-Shift-V in the guest.
                        window.subtitle = "Release shortcut keys to paste…"
                        while (NSApp as? WardenApplication)?.pasteInput.released != true {
                            try Task.checkCancellation()
                            guard ProcessInfo.processInfo.systemUptime < deadline else { throw PasteError.timeout }
                            try await Task.sleep(nanoseconds: 20_000_000)
                        }
                        guard pending == id, ready, window.firstResponder === view else {
                            if pending == id { cancel() }
                            return
                        }
                        pasteKey(into: view, window: window)
                        pending = nil; task = nil; window.subtitle = ""
                        return
                    }
                    guard ["pending", "collected"].contains(result["status"] as? String ?? "") else { throw PasteError.transfer }
                    try await Task.sleep(nanoseconds: 100_000_000)
                }
                cancel()
            } catch {
                guard pending == id else { return }
                cancel()
                window.subtitle = "Paste failed — check the guest clipboard receiver and try again"
                NSSound.beep()
            }
        }
    }
    func pasteKey(into view: VZVirtualMachineView, window: NSWindow) {
        // Send only to the virtual keyboard, never the host event queue. These
        // are fixed key codes, not key events derived from clipboard contents.
        func event(_ type: NSEvent.EventType, _ code: UInt16, _ flags: NSEvent.ModifierFlags, _ text: String = "") -> NSEvent {
            NSEvent.keyEvent(with: type, location: .zero, modifierFlags: flags,
                timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: window.windowNumber,
                context: nil, characters: text, charactersIgnoringModifiers: text,
                isARepeat: false, keyCode: code)!
        }
        // VZVirtualMachineView consumes physical key codes. flagsChanged alone
        // did not press Command in the guest on macOS 15; send its key down/up.
        view.keyDown(with: event(.keyDown, 55, .command))
        view.keyDown(with: event(.keyDown, 9, .command, "v"))
        view.keyUp(with: event(.keyUp, 9, .command, "v"))
        view.keyUp(with: event(.keyUp, 55, []))
    }
}

import AppKit
import Virtualization

final class RecordingVMView: VZVirtualMachineView {
    var keys: [(NSEvent.EventType, UInt16)] = []
    override func keyDown(with event: NSEvent) { keys.append((event.type, event.keyCode)) }
    override func keyUp(with event: NSEvent) { keys.append((event.type, event.keyCode)) }
    override func flagsChanged(with event: NSEvent) { fatalError("Modifier notification cannot replace a physical Command key press") }
}

@main
struct NativePasteCheck {
    @MainActor static func main() {
        let application = WardenApplication.shared
        precondition(application is WardenApplication, "The shortcut interceptor must own the application event dispatch")
        let window = GuestWindow(contentRect: NSRect(x: 0, y: 0, width: 200, height: 200), styleMask: .titled, backing: .buffered, defer: false)
        let view = RecordingVMView()
        let paste = HostPaste(view: view, window: window, state: URL(fileURLWithPath: "/nonexistent"))
        paste.pasteKey(into: view, window: window)
        precondition(view.keys.map { $0.1 } == [55, 9, 9, 55], "Command must be physically held while V is pressed")
        precondition(view.keys.map { $0.0 } == [.keyDown, .keyDown, .keyUp, .keyUp])
        func key(_ type: NSEvent.EventType, _ code: UInt16, _ flags: NSEvent.ModifierFlags = [], repeat repeating: Bool = false) -> NSEvent {
            NSEvent.keyEvent(with: type, location: .zero, modifierFlags: flags, timestamp: 0,
                windowNumber: window.windowNumber, context: nil, characters: "", charactersIgnoringModifiers: "",
                isARepeat: repeating, keyCode: code)!
        }
        var gate = PasteInputGate()
        precondition(gate.handle(key(.keyDown, 9, [.command, .shift]), inGuest: true) == .start)
        precondition(!gate.released)
        precondition(gate.handle(key(.keyDown, 9, [.command, .shift], repeat: true), inGuest: true) == .consume)
        precondition(gate.handle(key(.keyUp, 9, [.command, .shift]), inGuest: true) == .consume)
        precondition(!gate.released, "V release alone must not allow a paste with modifiers still down")
        let wakeup = NSEvent.otherEvent(with: .applicationDefined, location: .zero, modifierFlags: [],
            timestamp: 0, windowNumber: window.windowNumber, context: nil, subtype: 0, data1: 0, data2: 0)!
        _ = gate.handle(wakeup, inGuest: true)
        precondition(!gate.released, "A non-keyboard event must not release held modifiers")
        precondition(gate.handle(key(.flagsChanged, 56, [.command]), inGuest: true) == .forward)
        precondition(!gate.released)
        precondition(gate.handle(key(.flagsChanged, 55), inGuest: true) == .forward)
        precondition(gate.released, "Physical release must unblock automatic paste without another key press")
        precondition(gate.handle(key(.keyDown, 9, [.command]), inGuest: true) == .cancelAndForward,
            "Manual Cmd-V must cancel the automatic paste before Virtualization receives it")
        precondition(gate.handle(key(.keyDown, 0), inGuest: true) == .cancelAndForward)
        precondition(gate.handle(key(.keyDown, 9, [.command, .shift]), inGuest: false) == .forward)
        gate.reset()
        precondition(gate.released)
        // Releasing modifiers before V is equally valid; do not leak an unmatched
        // V key-up to the guest or start another transfer for a held shortcut.
        precondition(gate.handle(key(.keyDown, 9, [.command, .shift]), inGuest: true) == .start)
        _ = gate.handle(key(.flagsChanged, 55), inGuest: true)
        precondition(!gate.released)
        precondition(gate.handle(key(.keyUp, 9), inGuest: true) == .consume)
        precondition(gate.released)
        print("Native paste checks passed: balanced shortcut/release, repeat suppression, manual-paste cancellation, fixed VM key sequence.")
    }
}

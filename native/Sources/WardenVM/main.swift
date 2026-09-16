import AppKit
import Virtualization
import Darwin

let fm = FileManager.default
let args = Array(CommandLine.arguments.dropFirst())
let defaultRoot = Bundle.main.bundleURL.pathExtension == "app" ? Bundle.main.bundleURL.deletingLastPathComponent().path : fm.currentDirectoryPath + "/.local"
let root = URL(fileURLWithPath: ProcessInfo.processInfo.environment["WARDEN_STATE"] ?? defaultRoot, isDirectory: true)
let command = args.first ?? (Bundle.main.bundleIdentifier == "dev.warden.menu" ? "menu" : ((try? String(contentsOf: root.appendingPathComponent("launch-mode")))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "run"))
func file(_ name: String) -> URL { root.appendingPathComponent(name) }
let guestWindowSize = NSSize(width: 1152, height: 720)
func fail(_ message: String) -> Never { fputs("warden-vm: \(message)\n", stderr); exit(1) }
func emit(_ object: [String: Any]) { if let data = try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys]), let s = String(data: data, encoding: .utf8) { print(s); fflush(stdout) } }
func disk(_ url: URL, size: Int64) throws {
    guard !fm.fileExists(atPath: url.path) else { return }
    let fd = Darwin.open(url.path, O_CREAT | O_EXCL | O_RDWR, 0o600)
    guard fd >= 0 else { throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno)) }
    defer { Darwin.close(fd) }
    guard ftruncate(fd, size) == 0 else { throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno)) }
}
func block(_ url: URL, readOnly: Bool = false) throws -> VZVirtioBlockDeviceConfiguration {
    // Explicit host caching and durable guest flushes. Bad cached reads were observed
    // with automatic mode; use explicit caching as a mitigation under validation.
    VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(url: url, readOnly: readOnly, cachingMode: .cached, synchronizationMode: .full))
}
func nic(_ attachment: VZNetworkDeviceAttachment, _ mac: String) -> VZVirtioNetworkDeviceConfiguration {
    let n = VZVirtioNetworkDeviceConfiguration(); n.attachment = attachment; n.macAddress = VZMACAddress(string: mac)!; return n
}
func linux(_ link: FileHandle?) throws -> VZVirtualMachineConfiguration {
    let c = VZVirtualMachineConfiguration(); c.cpuCount = 2; c.memorySize = 4 * 1024 * 1024 * 1024
    let boot = VZEFIBootLoader()
    let efi = file("proxy/efi.bin")
    boot.variableStore = fm.fileExists(atPath: efi.path) ? VZEFIVariableStore(url: efi) : try VZEFIVariableStore(creatingVariableStoreAt: efi)
    c.bootLoader = boot
    c.storageDevices = try [block(file("proxy/disk.raw")), block(file("proxy/seed.iso"), readOnly: true)]
    c.networkDevices = [nic(VZNATNetworkDeviceAttachment(), "02:57:41:00:00:01")]
    if let link { c.networkDevices.append(nic(VZFileHandleNetworkDeviceAttachment(fileHandle: link), "02:57:41:00:00:02")) }
    c.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    c.socketDevices = [VZVirtioSocketDeviceConfiguration()]
    let graphics = VZVirtioGraphicsDeviceConfiguration()
    graphics.scanouts = [VZVirtioGraphicsScanoutConfiguration(widthInPixels: 1024, heightInPixels: 768)]
    c.graphicsDevices = [graphics]
    c.keyboards = [VZUSBKeyboardConfiguration()]
    c.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]
    let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
    serial.attachment = try VZFileSerialPortAttachment(url: file("proxy/console.log"), append: true)
    c.serialPorts = [serial]
    let share = VZVirtioFileSystemDeviceConfiguration(tag: "warden-assets")
    share.share = VZSingleDirectoryShare(directory: VZSharedDirectory(url: file("assets"), readOnly: true))
    c.directorySharingDevices = [share]
    try c.validate(); return c
}
func mac(_ link: FileHandle?, requirements: VZMacOSConfigurationRequirements? = nil) throws -> VZVirtualMachineConfiguration {
    let c = VZVirtualMachineConfiguration(); c.cpuCount = max(4, requirements?.minimumSupportedCPUCount ?? 4)
    c.memorySize = max(12 * 1024 * 1024 * 1024, requirements?.minimumSupportedMemorySize ?? 0)
    c.bootLoader = VZMacOSBootLoader()
    let p = VZMacPlatformConfiguration()
    guard let hardware = VZMacHardwareModel(dataRepresentation: try Data(contentsOf: file("mac/hardware.bin"))), hardware.isSupported else { throw NSError(domain: "Unsupported Mac hardware model", code: 1) }
    p.hardwareModel = hardware
    guard let identifier = VZMacMachineIdentifier(dataRepresentation: try Data(contentsOf: file("mac/machine.bin"))) else { throw NSError(domain: "Invalid Mac machine identifier", code: 1) }
    p.machineIdentifier = identifier
    p.auxiliaryStorage = VZMacAuxiliaryStorage(contentsOf: file("mac/auxiliary.bin"))
    c.platform = p
    c.storageDevices = try [block(file("mac/disk.raw"))]
    // The untrusted Mac gets exactly one private NIC. No NAT, bridge, vsock, USB,
    // host clipboard, host audio input, or host directory sharing is configured.
    if let link { c.networkDevices = [nic(VZFileHandleNetworkDeviceAttachment(fileHandle: link), "02:57:41:00:00:03")] }
    let graphics = VZMacGraphicsDeviceConfiguration()
    if let screen = NSScreen.main {
        graphics.displays = [VZMacGraphicsDisplayConfiguration(for: screen, sizeInPoints: guestWindowSize)]
    } else {
        graphics.displays = [VZMacGraphicsDisplayConfiguration(widthInPixels: 2304, heightInPixels: 1440, pixelsPerInch: 220)]
    }
    c.graphicsDevices = [graphics]
    c.keyboards = [VZUSBKeyboardConfiguration()]
    c.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]
    c.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    try c.validate(); return c
}

// This socket listener exists ONLY on the trusted Linux VM. It relays to a
// filesystem-protected host socket, never to the human approval HTTP server.
final class ControlBridge: NSObject, VZVirtioSocketListenerDelegate {
    func listener(_ listener: VZVirtioSocketListener, shouldAcceptNewConnection connection: VZVirtioSocketConnection, from socketDevice: VZVirtioSocketDevice) -> Bool {
        let target = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard target >= 0 else { return false }
        var addr = sockaddr_un(); addr.sun_family = sa_family_t(AF_UNIX)
        let path = file("control.sock").path.utf8CString
        guard path.count <= MemoryLayout.size(ofValue: addr.sun_path) else { Darwin.close(target); return false }
        withUnsafeMutableBytes(of: &addr.sun_path) { dest in path.withUnsafeBytes { src in dest.copyBytes(from: src) } }
        let ok = withUnsafePointer(to: &addr) { ptr in ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(target, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        guard ok == 0 else { Darwin.close(target); return false }
        // One request/response per connection; bound lifetime even if a client stalls.
        DispatchQueue.global().async {
            let source = connection.fileDescriptor
            var descriptors = [pollfd(fd: source, events: Int16(POLLIN), revents: 0), pollfd(fd: target, events: Int16(POLLIN), revents: 0)]
            var buffer = [UInt8](repeating: 0, count: 65536)
            defer { Darwin.close(target); connection.close() }
            while Darwin.poll(&descriptors, 2, 15000) > 0 {
                for i in 0..<2 where descriptors[i].revents != 0 {
                    let n = Darwin.read(descriptors[i].fd, &buffer, buffer.count)
                    if n <= 0 { return }
                    var offset = 0
                    while offset < n {
                        let written = buffer.withUnsafeBytes { Darwin.write(descriptors[1-i].fd, $0.baseAddress!.advanced(by: offset), n-offset) }
                        if written <= 0 { return }; offset += written
                    }
                }
            }
        }
        return true
    }
}

final class Runner: NSObject, NSApplicationDelegate, VZVirtualMachineDelegate, NSWindowDelegate {
    var hostPaste: HostPaste?
    var statusMenu: StatusMenu?
    var proxy: VZVirtualMachine?; var guest: VZVirtualMachine?; var window: NSWindow?
    var links: [FileHandle] = []; let bridge = ControlBridge(); let listener = VZVirtioSocketListener()
    var installer: VZMacOSInstaller?; var observation: NSKeyValueObservation?; var lockFD: Int32 = -1
    func startManagement(_ device: VZVirtioSocketDevice) {
        let path = file("vm-management.sock").path
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { fail("maintenance socket failed") }
        var addr = sockaddr_un(); addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = path.utf8CString
        guard bytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else { fail("state path too long") }
        withUnsafeMutableBytes(of: &addr.sun_path) { dest in bytes.withUnsafeBytes { dest.copyBytes(from: $0) } }
        unlink(path)
        let bound = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        guard bound == 0 else { fail("maintenance bind failed") }
        chmod(path, 0o600); Darwin.listen(fd, 8)
        DispatchQueue.global().async {
            while true {
                let client = Darwin.accept(fd, nil, nil)
                if client < 0 { return }
                DispatchQueue.main.async {
                    device.connect(toPort: 7001) { result in
                        switch result {
                        case .failure: Darwin.close(client)
                        case .success(let connection):
                            DispatchQueue.global().async {
                                var descriptors = [pollfd(fd: client, events: Int16(POLLIN), revents: 0), pollfd(fd: connection.fileDescriptor, events: Int16(POLLIN), revents: 0)]
                                var buffer = [UInt8](repeating: 0, count: 65536)
                                defer { Darwin.close(client); connection.close() }
                                while Darwin.poll(&descriptors, 2, 190000) > 0 {
                                    for i in 0..<2 where descriptors[i].revents != 0 {
                                        let n = Darwin.read(descriptors[i].fd, &buffer, buffer.count)
                                        if n <= 0 { return }
                                        var offset = 0
                                        while offset < n {
                                            let count = buffer.withUnsafeBytes { Darwin.write(descriptors[1-i].fd, $0.baseAddress!.advanced(by: offset), n-offset) }
                                            if count <= 0 { return }; offset += count
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    func applicationDidFinishLaunching(_ notification: Notification) {
        do {
            try fm.createDirectory(at: root, withIntermediateDirectories: true)
            if command == "menu" {
                lockFD = Darwin.open(file("menu.lock").path, O_CREAT | O_RDWR, 0o600)
                guard lockFD >= 0 else { fail("cannot open menu lock") }
                guard flock(lockFD, LOCK_EX | LOCK_NB) == 0 else { exit(0) }
                let project = URL(fileURLWithPath: ProcessInfo.processInfo.environment["WARDEN_PROJECT_ROOT"] ?? root.deletingLastPathComponent().path)
                MainActor.assumeIsolated { self.statusMenu = StatusMenu(state: root, project: project) }
                return
            }
            lockFD = Darwin.open(file("vm.lock").path, O_CREAT | O_RDWR, 0o600)
            guard lockFD >= 0, flock(lockFD, LOCK_EX | LOCK_NB) == 0 else { fail("another VM launcher is using this state directory") }
            if ["run", "proxy"].contains(command) { writeVMStatus("Starting") }
            if command == "restore-info" {
                VZMacOSRestoreImage.fetchLatestSupported { result in
                    switch result { case .failure(let error): fail(error.localizedDescription)
                    case .success(let image): emit(["url": image.url.absoluteString, "build": image.buildVersion, "version": "\(image.operatingSystemVersion.majorVersion).\(image.operatingSystemVersion.minorVersion).\(image.operatingSystemVersion.patchVersion)"]); exit(0) }
                }; return
            }
            if command == "install-mac" {
                guard args.count == 2 else { fail("install-mac requires a local IPSW path") }
                guard !fm.fileExists(atPath: file("mac/disk.raw").path) else { fail("Mac disk already exists; refusing to overwrite it") }
                let ipsw = URL(fileURLWithPath: args[1])
                VZMacOSRestoreImage.load(from: ipsw) { result in DispatchQueue.main.async {
                    do {
                        let image = try result.get()
                        guard let requirements = image.mostFeaturefulSupportedConfiguration else { fail("restore image is unsupported on this host") }
                        try fm.createDirectory(at: file("mac"), withIntermediateDirectories: true)
                        try requirements.hardwareModel.dataRepresentation.write(to: file("mac/hardware.bin"))
                        try VZMacMachineIdentifier().dataRepresentation.write(to: file("mac/machine.bin"))
                        _ = try VZMacAuxiliaryStorage(creatingStorageAt: file("mac/auxiliary.bin"), hardwareModel: requirements.hardwareModel, options: [])
                        try disk(file("mac/disk.raw"), size: 128 * 1024 * 1024 * 1024)
                        let vm = VZVirtualMachine(configuration: try mac(nil, requirements: requirements)); self.guest = vm
                        let install = VZMacOSInstaller(virtualMachine: vm, restoringFromImageAt: ipsw); self.installer = install
                        self.observation = install.progress.observe(\.fractionCompleted, options: [.new]) { progress, _ in emit(["installation_progress": progress.fractionCompleted]) }
                        install.install { result in
                            switch result { case .failure(let error): fail(error.localizedDescription)
                            case .success: try? Data("installed\n".utf8).write(to: file("mac/installed")); emit(["installed": true]); exit(0) }
                        }
                    } catch { fail(error.localizedDescription) }
                }}; return
            }
            var fds: [Int32] = [0, 0]
            try? fm.removeItem(at: file("proxy-ready.json"))
            guard socketpair(AF_UNIX, SOCK_DGRAM, 0, &fds) == 0 else { fail("socketpair failed") }
            links = fds.map { FileHandle(fileDescriptor: $0, closeOnDealloc: true) }
            let proxyVM = VZVirtualMachine(configuration: try linux(links[0])); proxyVM.delegate = self; proxy = proxyVM
            listener.delegate = bridge
            (proxyVM.socketDevices.first as! VZVirtioSocketDevice).setSocketListener(listener, forPort: 7000)
            proxyVM.start { result in
                switch result { case .failure(let error): fail(error.localizedDescription)
                case .success:
                    emit(["proxy": "running", "memory_gib": 4])
                    if command == "proxy" { self.writeVMStatus("Running") }
                    self.startManagement(proxyVM.socketDevices.first as! VZVirtioSocketDevice)
                    if command == "proxy" {
                        let view = VZVirtualMachineView(); view.virtualMachine = proxyVM
                        let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1024, height: 768), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
                        w.title = "Warden — trusted Linux appliance"; w.contentView = view; w.delegate = self; w.center(); w.makeKeyAndOrderFront(nil); self.window = w
                        NSApp.activate(ignoringOtherApps: true)
                    }
                    if command == "run" { self.waitForProxy(attempt: 0) }
                }
            }
        } catch { fail(error.localizedDescription) }
    }
    func waitForProxy(attempt: Int) {
        // Host service removes this marker on each start and writes it only on
        // receipt of the firewall/proxy readiness heartbeat over Linux vsock.
        let ready = file("proxy-ready.json")
        if let data = try? Data(contentsOf: ready), let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any], let time = object["time"] as? Double, Date().timeIntervalSince1970 - time < 15 {
            do {
                guard fm.fileExists(atPath: file("mac/installed").path) else { fail("install the macOS guest first") }
                let vm = VZVirtualMachine(configuration: try mac(links[1])); guest = vm; vm.delegate = self
                // Keep host shortcuts in AppKit, including Paste from Host.
                // capturesSystemKeys also lost modifier events on the tested host.
                let view = VZVirtualMachineView(); view.virtualMachine = vm; view.capturesSystemKeys = false
                view.automaticallyReconfiguresDisplay = true
                let w = GuestWindow(contentRect: NSRect(origin: .zero, size: guestWindowSize), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
                w.collectionBehavior.insert(.fullScreenPrimary)
                w.title = "Warden — isolated macOS guest"; w.contentView = view; w.delegate = self; w.center(); w.makeKeyAndOrderFront(nil); window = w
                MainActor.assumeIsolated {
                let paste = HostPaste(view: view, window: w, state: root); hostPaste = paste; w.hostPaste = paste
                let menu = NSMenu(); let appItem = NSMenuItem(); menu.addItem(appItem)
                let appMenu = NSMenu(title: "Warden"); appItem.submenu = appMenu
                let pasteItem = NSMenuItem(title: "Paste from Host", action: #selector(HostPaste.pasteFromHost(_:)), keyEquivalent: "v")
                pasteItem.keyEquivalentModifierMask = [.command, .shift]; pasteItem.target = paste
                appMenu.addItem(pasteItem); NSApp.mainMenu = menu
                }
                NSApp.activate(ignoringOtherApps: true)
                vm.start { result in
                    if case .failure(let error) = result { fail(error.localizedDescription) }
                    emit(["mac": "running", "memory_gib": 12])
                    self.writeVMStatus("Running")
                    self.monitorProxy(previous: data, lastChange: ProcessInfo.processInfo.systemUptime)
                }
            } catch { fail(error.localizedDescription) }
        } else if attempt < 600 {
            if attempt % 10 == 0 { emit(["waiting": "trusted Linux firewall and proxy readiness"]) }
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { self.waitForProxy(attempt: attempt+1) }
        } else { fail("proxy readiness timed out; inspect proxy/console.log") }
    }
    func monitorProxy(previous: Data, lastChange: TimeInterval) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            let current = try? Data(contentsOf: file("proxy-ready.json"))
            let now = ProcessInfo.processInfo.systemUptime
            let changed = current != nil && current != previous
            let last = changed ? now : lastChange
            if now - last > 20 {
                emit(["error": "Proxy heartbeat lost; pausing macOS. Inspect the proxy and restart the pair."])
                guard let guest = self.guest, guest.canPause else { fail("cannot pause guest after proxy heartbeat loss") }
                guest.pause { result in
                    if case .failure(let error) = result { fail(error.localizedDescription) }
                    emit(["mac": "paused", "reason": "proxy heartbeat lost"])
                    self.writeVMStatus("Paused")
                }
                return
            }
            self.monitorProxy(previous: current ?? previous, lastChange: last)
        }
    }
    func guestDidStop(_ virtualMachine: VZVirtualMachine) { fail("a VM stopped; closing the private network and stopping the pair") }
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) { fail(error.localizedDescription) }
    func windowShouldClose(_ sender: NSWindow) -> Bool {
        sender.orderOut(nil)
        return false
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        window?.deminiaturize(nil); window?.makeKeyAndOrderFront(nil)
        return true
    }
    func writeVMStatus(_ state: String) {
        let value: [String: Any] = ["pid": Int(getpid()), "state": state]
        if let data = try? JSONSerialization.data(withJSONObject: value) { try? data.write(to: file("vm-status.json"), options: .atomic) }
    }
}

signal(SIGPIPE, SIG_IGN)
// Prevent App Nap from suspending a headless/occluded launcher and its guests.
let activity = ["run", "proxy", "install-mac"].contains(command) ? ProcessInfo.processInfo.beginActivity(options: [.userInitiated, .idleSystemSleepDisabled], reason: "Running the Warden VM security boundary") : nil
if command == "doctor" {
    emit(["supported": VZVirtualMachine.isSupported, "physical_memory_gib": ProcessInfo.processInfo.physicalMemory / (1024*1024*1024), "minimum_host": "macOS 14 / Apple Silicon", "vm_memory_gib": 16]); exit(VZVirtualMachine.isSupported ? 0 : 1)
}
guard ["restore-info", "install-mac", "run", "proxy", "menu"].contains(command) else {
    print("warden-vm doctor | restore-info | install-mac PATH.ipsw | proxy | run | menu\nWARDEN_STATE selects the persistent state directory."); exit(0)
}
let application = WardenApplication.shared
application.setActivationPolicy(command == "menu" ? .accessory : (["run", "proxy"].contains(command) ? .regular : .prohibited))
let runner = Runner(); application.delegate = runner; application.run()

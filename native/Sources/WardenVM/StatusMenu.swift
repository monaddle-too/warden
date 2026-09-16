import AppKit
import Darwin
import UserNotifications

struct VMResourceStatus: Decodable {
    let status: String
    let sampled_at: Double?
    let cpu_percent: Double?
    let memory_used_bytes: Double?
    let memory_total_bytes: Double?
    let vcpu_count: Int?

    func summary(_ name: String, at now: Double) -> String {
        guard status == "ok", let sampled = sampled_at, (0...20).contains(now - sampled),
              let cpu = cpu_percent, cpu.isFinite, (0...100).contains(cpu),
              let used = memory_used_bytes, let total = memory_total_bytes,
              total.isFinite, total > 0, used.isFinite, (0...total).contains(used) else {
            return "\(name): \(status == "sampling" ? "Sampling…" : "Usage unavailable")"
        }
        return String(format: "%@: CPU %.1f%% · RAM %.1f / %.1f GiB", name, cpu, used / 1073741824, total / 1073741824)
    }
}

struct MenuControlStatus: Decodable {
    let proxy_ready: Bool
    let pending_count: Int?
    let requests: [Request]?
    let network_enabled: Bool?
    let network_applied: Bool?
    let resources: [String: VMResourceStatus]?
    struct Request: Decodable {
        let status: String
        let id: String?
        let repository: String?
        let operation: String?
    }
    var pending: Int { max(0, pending_count ?? requests?.filter { $0.status == "pending" }.count ?? 0) }
    enum CodingKeys: String, CodingKey { case proxy_ready, pending_count, requests, network_enabled, network_applied, resources }
    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        proxy_ready = try values.decode(Bool.self, forKey: .proxy_ready)
        resources = try values.decodeIfPresent([String: VMResourceStatus].self, forKey: .resources)
        network_enabled = try values.decodeIfPresent(Bool.self, forKey: .network_enabled)
        network_applied = try values.decodeIfPresent(Bool.self, forKey: .network_applied)
        pending_count = try values.decodeIfPresent(Int.self, forKey: .pending_count)
        requests = try values.decodeIfPresent([Request].self, forKey: .requests)
        guard (pending_count != nil || requests != nil), (pending_count ?? 0) >= 0 else {
            throw DecodingError.dataCorruptedError(forKey: .pending_count, in: values, debugDescription: "Approval count unavailable")
        }
    }
}

struct MenuSIEMStatus: Decodable {
    let updated: Double
    let state: String
    let unshipped_bytes: Int64?
    let error: String?

    func isFresh(at now: Double) -> Bool { (0...90).contains(now - updated) }
}

struct MenuSummary {
    let control: MenuControlStatus?
    let vmActive: Bool
    let siemConfigured: Bool
    let siemRunning: Bool
    let siem: MenuSIEMStatus?
    let now: Double

    var protection: String {
        guard let control else { return "Protection: Status unavailable" }
        guard vmActive else { return "Protection: VM stopped" }
        if control.network_enabled == false {
            return control.network_applied == false ? "Protection: Network disconnected" : "Protection: Disconnecting network…"
        }
        return control.proxy_ready ? "Protection: Proxy enforcing" : "Protection: Awaiting proxy heartbeat"
    }
    var needsAttention: Bool { control == nil || (vmActive && control?.proxy_ready != true) }
    var approvals: String {
        guard let control else { return "Approvals: Unavailable" }
        return "Approvals: \(control.pending) pending"
    }
    var delivery: String {
        guard siemConfigured else { return "SIEM: Not configured" }
        guard siemRunning, let siem, siem.isFresh(at: now), ["running", "pending", "retrying"].contains(siem.state) else { return "SIEM: Shipper unavailable" }
        guard let bytes = siem.unshipped_bytes, bytes >= 0 else { return "SIEM: Status unavailable" }
        if siem.error != nil || siem.state == "retrying" || bytes > 0 {
            let size = ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file)
            return "SIEM: \(siem.error != nil || siem.state == "retrying" ? "Retrying" : "Sending") · \(size) buffered"
        }
        return "SIEM: Up to date"
    }
}

// Read only the authenticated host state. No approvals are issued by this menu.
final class MenuStatusTransport: NSObject, URLSessionTaskDelegate {
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
    func mutate(_ path: String, value: [String: Any]) async throws {
        let token = try String(contentsOf: state.appendingPathComponent("admin-token"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        var request = URLRequest(url: URL(string: "http://127.0.0.1:18765/api/" + path)!)
        request.httpMethod = "POST"
        request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization")
        request.setValue("http://127.0.0.1:18765", forHTTPHeaderField: "Origin")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: value)
        let (_, response) = try await session.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw URLError(.badServerResponse) }
    }
    func fetch() async throws -> MenuControlStatus {
        let token = try String(contentsOf: state.appendingPathComponent("admin-token"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        var request = URLRequest(url: URL(string: "http://127.0.0.1:18765/api/state")!)
        request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization")
        let (bytes, response) = try await session.bytes(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw URLError(.badServerResponse) }
        var data = Data()
        for try await byte in bytes {
            guard data.count < 2 * 1024 * 1024 else { throw URLError(.dataLengthExceedsMaximum) }
            data.append(byte)
        }
        return try JSONDecoder().decode(MenuControlStatus.self, from: data)
    }
}

@MainActor
final class StatusMenu: NSObject, NSMenuDelegate, UNUserNotificationCenterDelegate {
    let state: URL
    let project: URL
    let transport: MenuStatusTransport
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    let vmItem = NSMenuItem(title: "VM: Checking…", action: nil, keyEquivalent: "")
    let macResourceItem = NSMenuItem(title: "macOS: Checking usage…", action: nil, keyEquivalent: "")
    let proxyResourceItem = NSMenuItem(title: "Proxy: Checking usage…", action: nil, keyEquivalent: "")
    let protectionItem = NSMenuItem(title: "Protection: Checking…", action: nil, keyEquivalent: "")
    let approvalsItem = NSMenuItem(title: "Approvals: Checking…", action: nil, keyEquivalent: "")
    let deliveryItem = NSMenuItem(title: "SIEM: Checking…", action: nil, keyEquivalent: "")
    var showItem: NSMenuItem!
    var startItem: NSMenuItem!
    var reviewItem: NSMenuItem!
    var networkItem: NSMenuItem!
    var notificationItem: NSMenuItem!
    var notified = Set<String>()
    var groupDates: [String: Double] = [:]
    var lastNotificationAt: Double = 0
    var notificationsEnabled = false
    var deliveredNotificationCount = 0
    var notificationPermission = 0
    var timer: Timer?
    var refreshing = false
    var control: MenuControlStatus?
    var launch: Process?

    init(state: URL, project: URL) {
        self.state = state; self.project = project
        transport = MenuStatusTransport(state: state)
        super.init()
        menu.autoenablesItems = false; menu.delegate = self
        let title = NSMenuItem(title: "Warden", action: nil, keyEquivalent: "")
        title.attributedTitle = NSAttributedString(string: "Warden", attributes: [.font: NSFont.boldSystemFont(ofSize: 13)])
        title.isEnabled = false; menu.addItem(title)
        for row in [vmItem, macResourceItem, proxyResourceItem, protectionItem, approvalsItem, deliveryItem] { row.isEnabled = false; menu.addItem(row) }
        menu.addItem(.separator())
        showItem = action("Show VM", #selector(showVM), symbol: "macwindow")
        startItem = action("Start VM", #selector(startVM), symbol: "play")
        menu.addItem(.separator())
        reviewItem = action("Review Requests…", #selector(openDashboard), symbol: "checklist")
        _ = action("Open Dashboard…", #selector(openDashboard), symbol: "slider.horizontal.3")
        _ = action("Revoke All Permissions", #selector(revokeAll), symbol: "hand.raised")
        networkItem = action("Disconnect Network", #selector(toggleNetwork), symbol: "network.slash")
        notificationItem = action("Enable Approval Notifications…", #selector(enableNotifications), symbol: "bell")
        _ = action("Open SIEM…", #selector(openSIEM), symbol: "chart.bar.xaxis")
        _ = action("Show Logs in Finder", #selector(showLogs), symbol: "folder")
        menu.addItem(.separator())
        _ = action("Quit Menu Bar", #selector(quit), symbol: nil)
        item.menu = menu
        item.button?.setAccessibilityLabel("Warden")
        render()
        let timer = Timer(timeInterval: 3, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.refresh() }
        }
        RunLoop.main.add(timer, forMode: .common); self.timer = timer
        UNUserNotificationCenter.current().delegate = self
        if let bytes = try? Data(contentsOf: state.appendingPathComponent("menu-notified.json")),
           let ids = try? JSONDecoder().decode([String].self, from: bytes) { notified = Set(ids.suffix(1000)) }
        notificationsEnabled = FileManager.default.fileExists(atPath: state.appendingPathComponent("notifications-enabled").path)
        refresh()
        let asked = state.appendingPathComponent("notifications-requested")
        if !FileManager.default.fileExists(atPath: asked.path) {
            try? Data("requested".utf8).write(to: asked, options: .atomic)
            enableNotifications()
        }
    }

    func action(_ title: String, _ selector: Selector, symbol: String?) -> NSMenuItem {
        let row = NSMenuItem(title: title, action: selector, keyEquivalent: "")
        row.target = self
        if let symbol { row.image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil) }
        menu.addItem(row); return row
    }

    func locked(_ name: String) -> Bool {
        let fd = Darwin.open(state.appendingPathComponent(name).path, O_RDWR)
        guard fd >= 0 else { return false }
        defer { Darwin.close(fd) }
        if flock(fd, LOCK_EX | LOCK_NB) == 0 { flock(fd, LOCK_UN); return false }
        return errno == EWOULDBLOCK
    }

    var vmApplication: NSRunningApplication? {
        NSRunningApplication.runningApplications(withBundleIdentifier: "dev.warden.vm").first {
            $0.bundleURL?.resolvingSymlinksInPath().standardizedFileURL == state.appendingPathComponent("Warden.app").resolvingSymlinksInPath().standardizedFileURL
        }
    }

    func render() {
        let vmActive = locked("vm.lock")
        let decoder = JSONDecoder()
        let siem = (try? Data(contentsOf: state.appendingPathComponent("siem-status.json"))).flatMap { try? decoder.decode(MenuSIEMStatus.self, from: $0) }
        let summary = MenuSummary(control: control, vmActive: vmActive,
                                  siemConfigured: FileManager.default.fileExists(atPath: state.appendingPathComponent("siem.json").path),
                                  siemRunning: locked("siem.lock"), siem: siem, now: Date().timeIntervalSince1970)
        var vmLabel = vmActive ? "Active" : "Stopped"
        if vmActive, let app = vmApplication,
           let data = try? Data(contentsOf: state.appendingPathComponent("vm-status.json")),
           let status = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           status["pid"] as? Int == Int(app.processIdentifier),
           let value = status["state"] as? String,
           ["Starting", "Running", "Paused", "Stopping"].contains(value) { vmLabel = value }
        vmItem.title = "VM: " + (launch != nil && !vmActive ? "Starting…" : vmLabel)
        protectionItem.title = summary.protection
        approvalsItem.title = summary.approvals
        deliveryItem.title = summary.delivery
        macResourceItem.title = control?.resources?["macos"]?.summary("macOS", at: summary.now) ?? "macOS: Usage unavailable"
        proxyResourceItem.title = control?.resources?["proxy"]?.summary("Proxy", at: summary.now) ?? "Proxy: Usage unavailable"
        reviewItem.title = control.map { $0.pending > 0 ? "Review \($0.pending) Pending Requests…" : "Review Requests…" } ?? "Review Requests…"
        networkItem.title = control?.network_enabled == false ? "Reconnect Network" : "Disconnect Network"
        networkItem.isEnabled = control != nil
        notificationItem.title = notificationsEnabled ? "Disable Approval Notifications" : "Enable Approval Notifications…"
        showItem.isEnabled = vmApplication != nil
        startItem.isEnabled = !vmActive && launch == nil && FileManager.default.isExecutableFile(atPath: project.appendingPathComponent("warden").path) && FileManager.default.fileExists(atPath: state.appendingPathComponent("mac/installed").path)
        let symbol = summary.needsAttention ? "exclamationmark.shield" : "shield.lefthalf.filled"
        let icon = NSImage(systemSymbolName: symbol, accessibilityDescription: "Warden")
        icon?.isTemplate = true; icon?.size = NSSize(width: 18, height: 18)
        item.button?.image = icon
        item.button?.title = control.map { $0.pending > 0 ? " \(min($0.pending, 999))\($0.pending > 999 ? "+" : "")" : "" } ?? ""
        item.button?.toolTip = "Warden · \(vmItem.title) · \(summary.approvals)"
        // A small, credential-free diagnostic snapshot for ./warden status.
        let snapshot: [String: Any] = ["updated": Date().timeIntervalSince1970,
                                      "vm": vmItem.title,
                                      "macos_resources": macResourceItem.title,
                                      "proxy_resources": proxyResourceItem.title, "protection": summary.protection,
                                      "approvals": summary.approvals, "siem": summary.delivery,
                                      "show_vm_available": showItem.isEnabled,
                                      "notifications_enabled": notificationsEnabled,
                                      "notification_permission": notificationPermission,
                                      "delivered_notifications": deliveredNotificationCount]
        if let data = try? JSONSerialization.data(withJSONObject: snapshot) {
            try? data.write(to: state.appendingPathComponent("menu-status.json"), options: .atomic)
        }
    }

    func refresh() {
        render()
        guard !refreshing else { return }
        refreshing = true
        UNUserNotificationCenter.current().getNotificationSettings { [weak self] settings in
            Task { @MainActor in self?.notificationPermission = settings.authorizationStatus.rawValue }
        }
        UNUserNotificationCenter.current().getDeliveredNotifications { [weak self] notices in
            Task { @MainActor in self?.deliveredNotificationCount = notices.count }
        }
        Task { [weak self] in
            guard let self else { return }
            do { control = try await transport.fetch(); notifyApprovals() }
            catch { control = nil } // Never leave stale approvals or an old healthy indicator.
            refreshing = false; render()
        }
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    @objc func showVM() {
        guard let app = vmApplication, let url = app.bundleURL else { return }
        let config = NSWorkspace.OpenConfiguration(); config.activates = true
        NSWorkspace.shared.openApplication(at: url, configuration: config)
    }

    @objc func startVM() {
        guard !locked("vm.lock"), launch == nil else { return }
        let process = Process(); process.executableURL = project.appendingPathComponent("warden")
        process.arguments = ["run"]; process.currentDirectoryURL = project
        var environment = ProcessInfo.processInfo.environment
        environment["WARDEN_STATE"] = state.path; environment["WARDEN_PROJECT_ROOT"] = project.path
        process.environment = environment
        let log = state.appendingPathComponent("menu-launch.log")
        if !FileManager.default.fileExists(atPath: log.path) { FileManager.default.createFile(atPath: log.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        do {
            let output = try FileHandle(forWritingTo: log); try output.seekToEnd()
            process.standardOutput = output; process.standardError = output
            process.terminationHandler = { [weak self] _ in
                try? output.close()
                DispatchQueue.main.async { self?.launch = nil; self?.refresh() }
            }
            try process.run(); launch = process; render()
        } catch { alert("Could not start the VM", "Check menu-launch.log in the Warden state folder.") }
    }

    @objc func openDashboard() { openRequest(nil) }
    func openRequest(_ requestID: String?) {
        guard let token = try? String(contentsOf: state.appendingPathComponent("admin-token"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines), !token.isEmpty else {
            alert("Dashboard unavailable", "Start the host control plane with ./warden control."); return
        }
        var fragment = URLComponents(); fragment.queryItems = [URLQueryItem(name: "session", value: token)]
        if let requestID, UUID(uuidString: requestID) != nil { fragment.queryItems?.append(URLQueryItem(name: "request", value: requestID)) }
        var url = URLComponents(string: "http://127.0.0.1:18765/")!
        url.percentEncodedFragment = fragment.percentEncodedQuery
        if let url = url.url { NSWorkspace.shared.open(url) }
    }

    @objc func revokeAll() {
        Task { do { try await transport.mutate("revoke-all", value: [:]); refresh() }
               catch { alert("Could not revoke permissions", "The controller did not confirm revocation. Check its status.") } }
    }
    @objc func toggleNetwork() {
        let enabled = control?.network_enabled == false
        Task { do { try await transport.mutate("network", value: ["enabled": enabled]); refresh() }
               catch { alert("Network change not confirmed", "Check the host controller. The menu will show when the appliance applies the change.") } }
    }
    @objc func enableNotifications() {
        if notificationsEnabled {
            notificationsEnabled = false
            try? FileManager.default.removeItem(at: state.appendingPathComponent("notifications-enabled"))
            UNUserNotificationCenter.current().removeAllDeliveredNotifications(); render(); return
        }
        Task {
            do {
                let granted = try await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound])
                if granted {
                    notificationsEnabled = true
                    try Data("enabled".utf8).write(to: state.appendingPathComponent("notifications-enabled"), options: .atomic)
                    refresh()
                } else { alert("Notifications are disabled", "Enable Warden Menu in macOS System Settings → Notifications, then try again.") }
            } catch { alert("Notifications unavailable", "macOS could not enable Warden notifications.") }
        }
    }
    func notifyApprovals() {
        guard notificationsEnabled, let requests = control?.requests else { return }
        let now = Date().timeIntervalSince1970
        guard now - lastNotificationAt >= 15 else { return }
        let pending = requests.filter { $0.status == "pending" && $0.id != nil && !notified.contains($0.id!) }
        let groups = Dictionary(grouping: pending) { ($0.repository ?? "GitHub") + " · " + ($0.operation ?? "request") }
        for (group, requests) in groups.sorted(by: { $0.key < $1.key }) where now - (groupDates[group] ?? 0) >= 60 {
            guard let first = requests.first, let id = first.id, UUID(uuidString: id) != nil else { continue }
            groupDates[group] = now
            lastNotificationAt = now
            let content = UNMutableNotificationContent()
            content.title = "Warden approval required"
            let scope = first.operation == "git/read" ? "Suggested: repository read for 1 hour." : "Suggested: one exact action, expires in 5 minutes."
            content.body = String(group.prefix(160)) + (requests.count > 1 ? " · \(requests.count) requests" : "") + "\n" + scope + " Review on the host."
            content.threadIdentifier = "warden-approvals"
            content.userInfo = ["request": id]
            let notification = UNNotificationRequest(identifier: "warden-" + id, content: content, trigger: nil)
            UNUserNotificationCenter.current().add(notification) { [weak self] error in
                guard error == nil else { return }
                Task { @MainActor in
                    guard let self else { return }
                    for request in requests { if let id = request.id { self.notified.insert(id) } }
                    if self.notified.count > 1000 { self.notified = Set(requests.compactMap { $0.id }) }
                    if let bytes = try? JSONEncoder().encode(Array(self.notified)) { try? bytes.write(to: self.state.appendingPathComponent("menu-notified.json"), options: .atomic) }
                }
            }
            break // Bound bursts across distinct repositories and operations too.
        }
    }
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse, withCompletionHandler completionHandler: @escaping () -> Void) {
        let id = response.notification.request.content.userInfo["request"] as? String
        Task { @MainActor in self.openRequest(id); completionHandler() }
    }
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification, withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .list])
    }

    @objc func openSIEM() { NSWorkspace.shared.open(URL(string: "https://siem.monaddle.com/")!) }
    @objc func showLogs() {
        let logs = ["control.log", "siem.log", "menu.log"].map { state.appendingPathComponent($0) }.filter { FileManager.default.fileExists(atPath: $0.path) }
        if logs.isEmpty { NSWorkspace.shared.open(state) }
        else { NSWorkspace.shared.activateFileViewerSelecting(logs) }
    }
    @objc func quit() { NSApp.terminate(nil) } // This helper never owns the VMs or control plane.
    func alert(_ title: String, _ detail: String) {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert(); alert.messageText = title; alert.informativeText = detail; alert.runModal()
    }
}

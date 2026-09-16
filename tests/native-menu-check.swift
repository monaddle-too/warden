import Foundation

@main
struct MenuChecks {
    static func main() async throws {
        let decoder = JSONDecoder()
        func control(_ json: String) throws -> MenuControlStatus { try decoder.decode(MenuControlStatus.self, from: Data(json.utf8)) }
        func siem(_ json: String) throws -> MenuSIEMStatus { try decoder.decode(MenuSIEMStatus.self, from: Data(json.utf8)) }
        let resource = try decoder.decode(VMResourceStatus.self, from: Data(#"{"status":"ok","sampled_at":100,"cpu_percent":25,"memory_used_bytes":1073741824,"memory_total_bytes":4294967296,"vcpu_count":2}"#.utf8))
        precondition(resource.summary("Proxy", at: 110).contains("CPU 25.0% · RAM 1.0 / 4.0 GiB"))
        precondition(resource.summary("Proxy", at: 121).contains("unavailable"))
        precondition(resource.summary("Proxy", at: 99).contains("unavailable"))
        let paged = try control(#"{"proxy_ready":true,"pending_count":40,"requests":[{"status":"pending"}]}"#)
        precondition(paged.pending == 40, "Use total approvals, not the first page size")
        let legacy = try control(#"{"proxy_ready":true,"requests":[{"status":"pending"},{"status":"denied"}]}"#)
        precondition(legacy.pending == 1)
        precondition((try? control(#"{"proxy_ready":true}"#)) == nil, "Missing count must not show zero")
        precondition((try? control(#"{"proxy_ready":true,"pending_count":-1}"#)) == nil)
        let retrying = try siem(#"{"updated":100,"state":"retrying","unshipped_bytes":4096,"error":"unreachable"}"#)
        let healthy = MenuSummary(control: paged, vmActive: true, siemConfigured: true, siemRunning: true, siem: retrying, now: 105)
        precondition(!healthy.needsAttention, "SIEM outage is separate from enforcement failure")
        precondition(healthy.delivery.contains("buffered") && healthy.delivery.contains("Retrying"))
        let stale = MenuSummary(control: nil, vmActive: true, siemConfigured: true, siemRunning: true, siem: retrying, now: 300)
        precondition(stale.needsAttention && stale.approvals.contains("Unavailable"))
        precondition(stale.delivery.contains("unavailable"))
        precondition(!retrying.isFresh(at: 99), "Future timestamps are not evidence of health")
        let dead = MenuSummary(control: paged, vmActive: true, siemConfigured: true, siemRunning: false, siem: retrying, now: 105)
        precondition(dead.delivery.contains("unavailable"), "A fresh file alone is not a running shipper")
        let missingHeartbeat = try control(#"{"proxy_ready":false,"pending_count":0}"#)
        let unprotected = MenuSummary(control: missingHeartbeat, vmActive: true, siemConfigured: false, siemRunning: false, siem: nil, now: 105)
        precondition(unprotected.needsAttention && unprotected.protection.contains("Awaiting"))
        let stopped = MenuSummary(control: paged, vmActive: false, siemConfigured: false, siemRunning: false, siem: nil, now: 105)
        precondition(stopped.protection.contains("stopped"), "Old ready state cannot mark a stopped VM as enforcing")
        let cutoff = try control(#"{"proxy_ready":true,"pending_count":0,"network_enabled":false,"network_applied":false}"#)
        let disconnected = MenuSummary(control: cutoff, vmActive: true, siemConfigured: false, siemRunning: false, siem: nil, now: 105)
        precondition(disconnected.protection.contains("disconnected"))
        let requested = try control(#"{"proxy_ready":true,"pending_count":0,"network_enabled":false,"network_applied":true}"#)
        let waiting = MenuSummary(control: requested, vmActive: true, siemConfigured: false, siemRunning: false, siem: nil, now: 105)
        precondition(waiting.protection.contains("Disconnecting"), "Never report cutoff before appliance acknowledgement")
        if CommandLine.arguments.count == 2 {
            let live = try await MenuStatusTransport(state: URL(fileURLWithPath: CommandLine.arguments[1])).fetch()
            print("Live host status verified: \(live.pending) pending approvals; proxy ready: \(live.proxy_ready)")
        }
        print("PASS: menu health, pagination, stale status, unavailable counts, and SIEM buffering")
    }
}

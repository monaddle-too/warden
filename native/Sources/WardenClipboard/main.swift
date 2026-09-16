import AppKit
import Foundation
import Darwin

// This executable is guest-only. It never reads a clipboard or executes text.
var modelBytes = [CChar](repeating: 0, count: 256)
var modelSize = modelBytes.count
guard sysctlbyname("hw.model", &modelBytes, &modelSize, nil, 0) == 0,
      String(cString: modelBytes).hasPrefix("VirtualMac") else { exit(1) }

final class Receiver: NSObject, URLSessionDataDelegate {
    var active = false
    var bytes = Data()
    var accepted = false
    var transferID: String?
    lazy var session: URLSession = {
        let config = URLSessionConfiguration.ephemeral
        config.urlCache = nil
        config.httpCookieStorage = nil
        config.urlCredentialStorage = nil
        config.connectionProxyDictionary = [:]
        config.timeoutIntervalForRequest = 3
        config.timeoutIntervalForResource = 5
        return URLSession(configuration: config, delegate: self, delegateQueue: .main)
    }()
    func poll() {
        guard !active else { return }
        active = true; accepted = false; bytes = Data(); transferID = nil
        var request = URLRequest(url: URL(string: "http://10.77.0.1:8081/clipboard")!)
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("1", forHTTPHeaderField: "X-Warden-Clipboard")
        request.setValue("2", forHTTPHeaderField: "X-Warden-Clipboard-Version")
        session.dataTask(with: request).resume()
    }
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        completionHandler(nil)
    }
    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse, completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        let http = response as? HTTPURLResponse
        transferID = http?.value(forHTTPHeaderField: "X-Warden-Transfer")
        accepted = http?.statusCode == 200 && response.expectedContentLength <= 65536 && transferID.flatMap(UUID.init(uuidString:)) != nil
        completionHandler(accepted ? .allow : .cancel)
    }
    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        guard bytes.count + data.count <= 65536 else { accepted = false; dataTask.cancel(); return }
        bytes.append(data)
    }
    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        defer { active = false; accepted = false; bytes = Data() }
        guard error == nil, accepted, let transferID, let text = String(data: bytes, encoding: .utf8) else { return }
        // Always plain text, even for strings resembling RTF, HTML, or scripts.
        NSPasteboard.general.clearContents()
        guard NSPasteboard.general.setString(text, forType: .string) else { return }
        var request = URLRequest(url: URL(string: "http://10.77.0.1:8081/clipboard/ack")!)
        request.httpMethod = "POST"
        request.httpBody = Data()
        request.setValue("0", forHTTPHeaderField: "Content-Length")
        request.setValue("1", forHTTPHeaderField: "X-Warden-Clipboard")
        request.setValue(transferID, forHTTPHeaderField: "X-Warden-Transfer")
        session.dataTask(with: request) { _, _, _ in }.resume()
    }
}
let receiver = Receiver()
let timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { _ in receiver.poll() }
RunLoop.main.run()

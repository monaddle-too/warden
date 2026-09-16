import Foundation
import Darwin

// Native executable so CLI code is covered by the app signature, without
// script-signature extended attributes that can be lost when an app is copied.
let executable = URL(fileURLWithPath: CommandLine.arguments[0]).standardizedFileURL
let resources = executable.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Resources")
let python = resources.appendingPathComponent("runtime/python/bin/python3").path
let environment = ProcessInfo.processInfo.environment
for name in environment.keys where name.hasPrefix("DYLD_") || name.hasPrefix("PYTHON") { unsetenv(name) }
setenv("PATH", resources.appendingPathComponent("runtime/bin").path + ":/usr/bin:/bin:/usr/sbin:/sbin", 1)
setenv("PYTHONHOME", resources.appendingPathComponent("runtime/python").path, 1)
setenv("PYTHONNOUSERSITE", "1", 1); setenv("PYTHONDONTWRITEBYTECODE", "1", 1)
setenv("WARDEN_PACKAGED", "1", 1); setenv("WARDEN_OFFLINE", "1", 1)
if environment["WARDEN_STATE"] == nil {
    let home = environment["HOME"] ?? NSHomeDirectory()
    setenv("WARDEN_STATE", home + "/Library/Application Support/Warden", 1)
}
let arguments = [python, resources.appendingPathComponent("payload/warden").path] + CommandLine.arguments.dropFirst()
let pointers = arguments.map { strdup($0) } + [nil]
execv(python, pointers)
fputs("Could not launch Warden's bundled Python runtime.\n", stderr)
exit(1)

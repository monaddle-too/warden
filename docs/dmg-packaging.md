# Local DMG packaging

Build using only locally installed tools and cached vendor inputs:

```sh
python3 scripts/package-dmg.py \
  --python-prefix /path/to/standalone/python \
  --qemu-img /opt/homebrew/bin/qemu-img \
  --cache "$PWD/.local"
```

The Python prefix must contain a standalone `bin/python3.12` (or another two-digit
minor version), its standard library, and its license. This Mac's locally cached
runtime is at
`/Users/danielporter/.cache/codex-runtimes/codex-primary-runtime/dependencies/python`.
The build never fetches packages, invokes npm, or downloads images. Swift, Apple
signing tools and hdiutil are required on the build machine, not the recipient.

Output: `dist/Warden-0.1.0-local-arm64.dmg` and a SHA256/size/platform JSON manifest.
The volume contains one Warden.app, an Applications shortcut and a local guide.
The app contains a native setup window and CLI, private VM/menu helpers, Python
without site-packages, the pinned Node runtime, and QEMU with its dylib closure.
Non-system QEMU load paths are rewritten relative to the bundled libraries.
The minimum macOS version is derived from the bundled binaries (15.0 here).
Components and the outer app are ad-hoc signed; they are not Apple-notarized.
The build validates deep signatures, external symlinks and a clean-environment
CLI launch before producing the compressed, read-only DMG.

The cached Node installer must match the checked-in vendor hash. The app's input
manifest pins the existing agent-tools archive, vendor installers and the Apple
and Ubuntu images. A recipient explicitly chooses an input cache; the importer
copies then verifies only named artifacts. It never imports private VM disks,
keys, credentials or repositories. The import uses APFS clones where available.
Preparation verifies inputs again, skips native rebuilding and npm, and refuses
missing inputs instead of downloading them. Missing downloads fail closed in the
underlying preparation commands when `WARDEN_OFFLINE=1`.

A fresh installation stores state in `~/Library/Application Support/Warden`.
For an isolated local test, launch the bundled CLI or setup executable with an
explicit `WARDEN_STATE`. Do not point it at the development checkout's existing
`.local`: runtime migration is deliberately refused. Active environments on port
18765 are detected before starting another. Guest pairing still requires the
fingerprint printed in that guest; it is never accepted automatically.

Replacing the app preserves state, but shut down its VMs and stop its services
before moving or replacing the bundle. Its CLI supports `services stop`.
The source/ZIP managed-release updater does not install DMGs or upgrade this app.
A dedicated app update channel remains future work.

## Remaining distribution work

- Developer ID signing and Apple notarization.
- Corresponding-source and third-party redistribution review before public release;
  available licenses and Homebrew SBOMs are included under Third Party Notices.
- A cached Linux apt/pip repository for a fully offline first proxy boot. This
  build's setup preparation is offline; first boot still provisions Linux packages
  from their pinned network sources. No VM was booted during local DMG validation.
- Fresh-machine macOS restore, first boot, pairing and Gatekeeper validation beyond
  the existing development environment. The local DMG does not bypass Gatekeeper.

## Simplified setup flow

The opening window has one primary action, **Set up Warden**. The offline backend
runs import/verification, image preparation and macOS installation in sequence.
Completed steps are detected on retry; an installed Mac needing tool staging gets
only that stage, never another restore. No VM is automatically booted. **Open
Warden** becomes the primary action after setup, and guest connection is requested
only once the guest is running. Existing controller conflicts still block starts.

**Customize…** holds installer/storage location choices and the optional
prepare-images-only mode. Locations are chosen explicitly or discovered in the
current state, `~/Library/Caches/Warden`, or a `Warden Inputs` folder beside the app.
Logs live in a separate **Details** window. Progress is driven by named backend
steps and actual macOS installer progress, not a simulated timer. The single
setup action is also available as `warden desktop setup [INPUT_FOLDER]`, with
`--prepare-only` to stop before restoring macOS.

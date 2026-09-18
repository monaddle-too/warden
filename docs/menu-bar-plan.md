# Warden in the macOS menu bar

Status: started 2026-09-18 on branch `feat/menu-bar` from origin/main
1b24571 (which holds `feat/background-service`, 3982852).

## Objective

A macOS Warden shows up where the owner looks for background services: an
item in the menu bar whose icon says at a glance whether Warden runs,
whether an agent is working and whether something waits on the owner, and
whose menu takes them to it in one click. `warden install` registers it
beside the service, it comes back at login, and it survives `warden stop`
so it can offer Start. The item is a thin renderer: everything it knows
comes from the `warden` binary, everything it does runs `warden`.

## What exists

- `cmd/warden/svc.go`: `serviceManager` with `launchdAgent` (plist under
  `~/Library/LaunchAgents/<label>.plist`, `launchctl bootstrap|bootout|
  kickstart|kill|print gui/<uid>`, commands through an injectable
  `commandRunner`) and `systemdUnit`; `serviceNames(state)` gives
  `com.monaddle.warden` for the default state, `com.monaddle.warden.<base>`
  otherwise; `cli.serviceFn` / `defaultServiceFn` for tests.
- `cmd/warden/service.go`: `registerService` (install the unit, wait for
  the chat endpoint), `startService`, `stopService`, `restartService`,
  `status`, `serviceCommand` (`warden service install|uninstall`).
- `cmd/warden/install.go` step 9 `ensureService` (idempotent; restarts on
  an upgrade); `uninstall.go` unregisters; `doctor.go` `serviceCheck`.
- `cmd/warden/notify.go`: `popups` follows the chat service's event stream
  with `tui.Watch` and opens the app on a chat with `tui.PopupURL`;
  `start.go` `launchURL` + `throughEdge` build the owner launch URL;
  `openBrowser` honours `$BROWSER`.
- `internal/tui/client.go`: `Client.Events` (SSE `GET /api/events`, full
  `State` snapshots, reconnects, 401 fatal), `Chat.Pending()`,
  `Approval.Summary()`, `Review.Summary(provider)`; `tui/activity.go`
  `ActivityLabel` (what the agent is doing, shared fixture with the web);
  `tui/status.go` `StatusLine`.
- `chats/spend.go`: `GET spend` → `SpendReport{Today, Week, All}` with
  `CostUSD`, `Turns`, `Priced`.
- Web: `ChatShell.tsx` selects the chat from `?chat=<id>`; the New chat
  form is `creating`.
- `scripts/release.sh`: cross-compiles `warden` for the three targets
  (`CGO_ENABLED=0`), stages `bin/`, `web/`, `config/`, `vendor/` per
  tarball; runs on the self-hosted Mac runner (`release.yml`).
  `scripts/deploy-local.sh` builds and unpacks a release into
  `~/.warden/releases/` and `warden restart`s.
- `native/`: the old VM product's Swift package, including
  `WardenVM/StatusMenu.swift`, a menu bar app against a dead control API.
  Precedent (AppKit `NSStatusItem`, `LSUIElement`), not reused.
- The Mac has Swift 6.1.2 (`xcrun swiftc`), so the runner does too.

## Shape

Two new pieces and a plist:

1. **`warden menu feed`** (Go, `cmd/warden/menu.go`): a long-running
   command that writes one JSON line per change to stdout — the menu's
   whole model. It follows the chat service's event stream when the
   endpoint answers and reports `service: stopped|starting|running`
   otherwise (asking the manager whether the unit runs while it is
   unreachable). Each line:
   `{"service":"running","state":"/Users/o/.warden","app":"<launch URL>",
   "attention":[{"chatID","chatTitle","kind":"approval|review|error",
   "label"}],"chats":[{"id","title","status","activity","idleFor"}],
   "spend":{"today":4.12,"turns":3,"priced":true}}`.
   Also `warden open --chat ID` and `warden open --new` so the menu opens
   the app on a chat or on the New chat form (`?new=1`, read by
   `ChatShell.tsx`).
2. **`warden-menu`** (Swift, `chat/menu/main.swift`, one bare arm64
   executable, no bundle): `NSApplication` with the accessory activation
   policy, an `NSStatusItem` with an SF Symbol whose variant is the
   state (stopped / running / working / attention, the attention count as
   the item's title), the dropdown built from the last feed line. It
   spawns `warden menu feed --config …` and restarts it after an exit;
   every action runs `warden` (`open`, `open --chat`, `open --new`,
   `start`, `stop`, `restart`) or Finder (`open <state>`). Quit quits the
   item only.
3. **launchd agent `<label>.menu`** (`com.monaddle.warden.menu`):
   `bin/warden-menu --warden <launcher> --config <warden.json>`,
   `RunAtLoad`, `KeepAlive` on unsuccessful exit, `LimitLoadToSessionType
   Aqua`. Registered by `warden install` (new step `menu bar`, `--menu=false`
   skips) and `warden service install`, unregistered by `warden service
   uninstall` and `warden uninstall`, restarted by `warden restart` and by
   an upgrade install; `warden stop` leaves it. macOS only; on Linux and
   on a macOS build without `bin/warden-menu` the step says so and passes.

The dropdown (final order):

```
Warden · running · 2 workspaces          (disabled)
─────
Approve: run command: go test ./… — Fix build     → open --chat
Review: pull request — Add tray                  → open --chat
Failed: Refactor auth — sandbox exited           → open --chat
─────
● Fix build — Running go test ./...              → open --chat
○ Add tray — idle 4 min                          → open --chat
  3 more…                                        → open
─────
New chat…                                        → open --new
Open Warden                                      → open
Today: $4.12 · 3 turns                           (disabled)
─────
Stop Warden / Start Warden
Restart Warden
Show Logs in Finder
Quit Menu Bar Item
```

"Needs you" and the chat list only appear when non-empty; chats shown are
the non-archived ones with a running turn first, then idle ones by recent
activity, eight at most.

## Steps

1. `warden menu feed`: the model (`menuState`), the follower (endpoint →
   `tui.Client.Events`; unreachable → manager status), JSON lines,
   tests over a fake chat service and a `fakeService`.
2. `warden open --chat ID | --new`; `ChatShell.tsx` opens the New chat
   form for `?new=1`.
3. `chat/menu/main.swift`: the status item, the dropdown, the feed
   process, the actions; built by hand with `swiftc` first.
4. `svc.go`: the menu agent (`launchdAgent` with a menu unit), `menuNames`,
   `platformMenu`, `cli.menuFn`; `install.go` step `menu bar`;
   `service.go` `service install|uninstall`, `restart`, `status`;
   `uninstall.go`; `doctor.go` `menu bar` check. Unit tests through the
   recording runner.
5. `scripts/release.sh`: compile `warden-menu` for darwin-arm64 with
   `swiftc` when it is present (the runner) and stage it into that
   tarball's `bin/`; `release.yml` unchanged (same script).
6. Docs: `docs/warden-local-install.md` (install step, §5, §8), README,
   `docs/feature-map.md` row and glossary; this plan.
7. Live: `scripts/deploy-local.sh` on a cloned home (`~/.warden-menu`),
   then the owner's `~/.warden`.

## Key decisions

1. **The Swift side renders, Go decides.** The feed reuses `tui.Client`,
   `Approval.Summary`, `Review.Summary`, `ActivityLabel` and the launch
   URL code the launcher's popups already use, so the menu's wording and
   the app's agree and the logic is tested in Go with the existing fakes.
   The Swift file needs no HTTP, no auth, no knowledge of the edge.
2. **No app bundle** (the owner's call): a bare executable can own a status
   item and use SF Symbols; it cannot post `UNUserNotificationCenter`
   notifications (those would come from Go through `osascript`, as
   `desktopNotify` does) and Login Items names it by the executable.
3. **Built on the Mac runner, shipped in the darwin tarball**, never
   compiled at install time: `warden install` stays free of a compiler
   dependency, and a build from a host without `swiftc` simply lacks the
   item (the install step reports it).
4. **A separate launchd agent, not a child of the launcher**, so the item
   outlives `warden stop` and can offer Start; `KeepAlive` on a crash
   only, so Quit sticks until the next login.
5. **No approve / deny in the menu.** A menu row cannot show a command in
   full; the row opens the chat on the approval and the app settles it.
6. **Label `<service label>.menu`**, so a cloned home's item
   (`com.monaddle.warden.<base>.menu`) coexists with the real one.

## Progress log

- 2026-09-18: plan written; worktree opened.
- 2026-09-18: steps 1–6 implemented (d3d7f0a, f1bb159, 835e6b8).
  `cmd/warden/menu.go` (`menuState`, `menuModel`, `menuFeeder`),
  `tui.Client.Stream` / `Spend`, `warden open --chat|--new` +
  `?new=1`; `chat/menu/main.swift`; `svc.go` `platformMenu` /
  `launchdAgent.menu` / `menuExecutable`; install step 10, `warden menu
  install|uninstall`, `service install|uninstall` cover both, `restart`
  restarts the item, `stop` leaves it, `status` + doctor rows, uninstall;
  `release.sh` compiles with `xcrun swiftc` and stages `bin/warden-menu`;
  docs. Unit tests: the model (order, cap, attention kinds, the one-hour
  failure window, archived ignored), the feeder against a fake chat
  service (stopped → starting → running lines, spend fetched once and
  again when a turn ends, stopped after the stream drops), the plist,
  install (register, idempotent, upgrade restart, `--menu=false`, no
  binary, no row off macOS), the commands, doctor.
- 2026-09-18: step 7, live on a cloned home `~/.warden-menu` (chat
  :18830; build 835e6b8 through `deploy-local.sh`, which built
  `warden-menu` and shipped it in the tarball): `warden install
  --upgrade` registered `com.monaddle.warden.warden-menu` and
  `….warden-menu.menu`, both running under launchd, `status` and
  `doctor` report both; the feed said `running` with the clone's two
  waiting reviews, `stopped` after `warden stop` with the item still up,
  `running` after `start`; `restart` restarted the item (new pid); a
  Claude turn showed `working: 1` with the startup stage ("installing:
  copying the Codex runtime bundle…"), then "Running sleep 20; echo
  done", then idle; `menu uninstall` / `menu install` / `service
  uninstall` (both agents gone, no orphaned feed); `warden uninstall`
  removed the clone. Before that the item ran by hand against the
  owner's `~/.warden` (read-only feed) and showed the shield with a red
  2; the owner's home was not changed. A screenshot could not be taken
  from the session (no screen-recording permission), so the dropdown's
  look is verified by the owner, not here.
- Remaining: deploy to the owner's `~/.warden` (`deploy-local.sh` then
  `warden menu install`, since deploy-local does not run install);
  later: notifications from the feed (Go, `osascript`), a bundle if
  Login Items' name (`warden-menu`) bothers.

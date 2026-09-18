# Warden as a user service

Status: started 2026-09-18 on branch `feat/background-service` from
origin/main 144d4f4.

## Objective

A desktop Warden behaves like any other service on the machine: `warden
install` registers it with the platform's user service manager and starts
it, it comes back at login (macOS) or boot (Linux, with lingering) and
after a crash, its logs do not grow without bound, and `warden
start`/`stop`/`restart`/`status` drive the manager instead of a pid file.
`--detach` stays as the fallback for hosts without a user service manager.

## What exists

- `cmd/warden/service.go`: `warden start --detach` forks `warden start
  --detached-child` into its own session (`Setsid`), writes
  `<state>/warden.pid`, appends stdout to `<state>/warden.log` and waits
  for the owner capability file to change. `warden stop` SIGTERMs the pid
  (`stopDetached` in `uninstall.go`); `warden status` reads the pid file.
- `cmd/warden/start.go`: the launcher (`launcher.run`) takes
  `<state>/launcher.lock` (flock), starts the private sbx daemon if it is
  down, runs the four services with output appended to
  `<state>/warden-<name>.log`, and exits (status 1, bug report drafted)
  when any of them exits. SIGTERM/SIGINT stop the stack in reverse order
  and the launcher exits 0. `detached` (from `--detached-child`) only
  changes how popups and bug-report drafts are presented.
- `cmd/warden/install.go`: numbered idempotent steps ending with the
  config write and "Installed. Next: …". The `cli` struct carries the
  streams and `openFn`/`notifyFn` for tests.
- `cmd/warden/doctor.go`: `doctorChecks` returns `check` rows; a missing
  provider login is a PASS with a hint.
- `cmd/warden/uninstall.go`: stops a detached Warden by pid, removes the
  sandboxes and the state.
- `os.Executable()` returns the path as invoked (through a symlink such as
  `~/.warden/release/bin/warden` it stays the symlink), verified on macOS.
- The owner's Mac already runs a Monaddle launchd agent
  (`com.monaddle.panta-runner`: `KeepAlive`, `RunAtLoad`,
  `ThrottleInterval`) — the house pattern.

## Steps

1. `service.go`: a `serviceManager` interface with a launchd
   implementation (macOS, `~/Library/LaunchAgents/<label>.plist`,
   `launchctl bootstrap|bootout|kickstart|kill|print gui/<uid>`) and a
   systemd user-unit implementation (Linux,
   `~/.config/systemd/user/<name>.service`, `systemctl --user`). Commands
   run through an injectable runner so tests see the sequence without
   launchctl.
2. `warden start --service`: the supervised mode (no pid file, stdout to
   the rotated `<state>/warden.log`, `detached` behaviour for popups and
   drafts).
3. Log rotation at launcher start: `warden.log` and the four service logs
   rotate at 10 MiB, three generations kept.
4. `warden install` step `service`: register and start (idempotent;
   `--service=false` skips; a host without a manager reports it and
   points at `--detach`). New `warden service install|uninstall` for
   doing it later or undoing it. `warden uninstall` unregisters first.
5. Manager-aware `start`/`stop`/`restart`/`status`: with a registered
   service they drive the manager; without one they behave as today.
   `warden restart` is new. Foreground `warden start` needs
   `--foreground` when a service is registered.
6. `warden doctor`: a `service` check (registered and pointing at this
   launcher, loaded; running or stopped as a detail).
7. Docs: `docs/warden-local-install.md` §5 and §8, README step 4,
   `docs/feature-map.md` row; `scripts/deploy-local.sh` restarts through
   `warden restart`.
8. Live test on a cloned home (`~/.warden-svc`, its own label), then the
   owner's `~/.warden` after the merge.

## Key decisions

1. A launchd **user agent**, not a LaunchDaemon: sbx keeps its Docker
   session in the login keychain, sandboxes are per user, and popups and
   `warden open` need the GUI session. So Warden starts at login on
   macOS; on Linux the unit is a user unit, and `loginctl enable-linger`
   (printed as a hint, not run: it can need authentication) makes it
   start at boot.
2. `KeepAlive = {SuccessfulExit = false}` with `ThrottleInterval 10`: a
   crash (the launcher exits 1 when a service dies) is restarted, a
   `warden stop` (SIGTERM, the launcher exits 0) is not, and the agent
   stays registered for the next login. systemd: `Restart=on-failure`,
   `RestartSec=5`.
3. The unit points at the launcher **as invoked** (`os.Executable()`
   unresolved). Installed through `~/.warden/release/bin/warden`, a new
   release only needs `warden restart`; installed from an unpacked
   tarball elsewhere, the service follows that directory and
   `warden service install` from the new one re-points it.
4. The label is `com.monaddle.warden` (unit `warden.service`) for the
   default state directory, `com.monaddle.warden.<basename>`
   (`warden-<basename>.service`) for any other, so cloned homes coexist.
5. Logs stay files under `<state>`, rotated by the launcher at start:
   launchd does not rotate and holds its own fd, so in service mode the
   launcher opens `warden.log` itself and the plist has no
   `StandardOutPath`. Within one run nothing is bounded; every restart
   is.
6. `warden start` with a registered service starts the service (that is
   what "start" means once the service exists); the terminal run is
   `--foreground`. `--detach` with a registered service is the same as
   the service start, with a note.

## Progress log

- 2026-09-18: plan written; worktree opened.
- 2026-09-18: steps 1–7 implemented (`cmd/warden/svc.go`, `service.go`,
  `install.go` step 9, `doctor.go`, `uninstall.go`, `start.go`
  `--service`/`--foreground`, docs, feature map, `deploy-local.sh`).
  Unit tests: unit rendering, the launchctl and systemctl command
  sequences against recording runners, the install step (register, idempotent
  re-run, upgrade restart, `--service=false`, no manager), the four commands
  and `warden service install|uninstall`, the doctor check, uninstall
  unregistering, log rotation. `TestMain` replaces the default manager so
  no test reaches launchctl.
- 2026-09-18: step 8, live on a cloned home `~/.warden-svc` (label
  `com.monaddle.warden.warden-svc`, build of this branch): `service
  install` registered and started the agent (the sbx daemon started under
  launchd, four services up, `warden chat list` answered); `stop` left it
  loaded with exit 0 and launchd did not restart it; `start` 1.8 s;
  `restart` 1.8 s on a settled run, 10 s right after a start (launchd's
  ThrottleInterval); `kill -9` of the launcher: launchd restarted it
  within the interval and killed the orphaned services with the process
  group (one launcher, four children afterwards); `install --upgrade`
  with an older record restarted the service on the new revision; `doctor`
  PASS; `service uninstall` then `start --detach` as the fallback; `service
  install` again and `warden uninstall --keep-state` unregistered it. Two
  fixes from the run: `awaitReady` does not trust "not running" in its
  first 3 s (launchd reports the unit before spawning it), and the upgrade
  restart captures the endpoint stamp before restarting. The owner's
  `~/.warden` untouched; clone removed. Linux unit unverified (no Linux
  host here), as the rest of the Linux path.
- Remaining: merge, deploy to `~/.warden` and register the real service
  (`warden install --upgrade` from the deployed release, or `warden service
  install`).

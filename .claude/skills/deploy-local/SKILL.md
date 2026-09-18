---
name: deploy-local
description: Build the current checkout as a release, install it into the operator's local Warden (~/.warden/release), restart the background Warden and smoke-test it from the terminal or browser. Use this whenever the user says deploy locally, deploy to ~/.warden, "so I can see it", "try it live", retry the live repro, or wants a branch running on this Mac for a manual check; also when they ask which build is currently deployed locally, or how to roll the local Warden back to an earlier build.
---

# Deploy the checkout to the local Warden

`scripts/deploy-local.sh` builds a release from HEAD, unpacks it beside
the previous ones under `~/.warden/releases/`, repoints the
`~/.warden/release` symlink and restarts the background Warden. Sessions
keep re-reading the script and `~/.warden` to relearn the same facts, so
they are collected here.

## Before deploying

`~/.warden` is one installation shared by every session on this Mac. A
deploy replaces whatever build another session left running, so first
see what is there:

```sh
readlink ~/.warden/release                    # which release directory is live
~/.warden/release/bin/warden version          # its revision
~/.warden/release/bin/warden status           # running? which service versions
git log --oneline -1                          # what you are about to deploy
```

If the live revision is not on your branch and the user did not ask for
the switch, say so before replacing it. Rolling back later is one
`ln -sfn` (below), so this is a courtesy, not a blocker.

## Deploy

```sh
WARDEN_HOME=$HOME/.warden/release scripts/deploy-local.sh 2>&1 | tail -15
```

Run it with the sandbox off: it runs the Go toolchain and pnpm, which the
Bash sandbox breaks (`go list std` returns nothing inside it). `GOPROXY=off
GOFLAGS=-mod=mod` are what the build needs offline; `release.sh` sets
them, so only add them when invoking `go` yourself.

- The default skips the test suite (`release.sh --skip-tests`) on the
  assumption you already ran the relevant tests; pass `--test` to run the
  whole suite first when the change is risky.
- `--no-restart` unpacks without restarting; useful when another session
  is mid-verification.
- The script prints `deployed <version> to ~/.warden/release` and the new
  `warden version` line, then stops and starts the background Warden. If
  no background Warden was running it says so; start one with
  `~/.warden/release/bin/warden start --detach`.
- Versions are `v0.0.0-dev.<12-char sha>`; uncommitted changes are not
  reflected in the version string, so commit first if the revision needs
  to be traceable.

## Smoke test

From the terminal, no browser needed:

```sh
W=~/.warden/release/bin/warden
$W status
ID=$($W chat new --provider claude "deploy smoke" | tail -1)
$W chat send --wait "$ID" "Run \`uname -a\` in the sandbox and report only the output."
$W chat list | head
```

`--provider codex` for the Codex path. `warden chat send` without
`--wait` returns immediately; `warden chat approve CHAT` answers a
pending approval. The first reply in a fresh workspace is slow because a
sandbox boots and the model thinks; it is not a deploy failure.

For a browser check, `~/.warden/release/bin/warden open --print` prints
the owner URL (with the sign-in token; never paste it into a document) and
`warden open` opens it.

## Where to look when it fails

- `~/.warden/warden.log` (launcher) and `~/.warden/warden-{chat,runner,policy,edge}.log`.
- Gateway audit records for a sandbox:
  `ls -t ~/.warden/policy/sandboxes/*/audit/events.jsonl | head -1`.
- `~/.warden/warden.json` is the configuration; `warden doctor` checks
  the host invariants when the failure looks environmental (SBX daemon,
  runtimes, logins). Run `sbx` and `warden` outside the sandbox too: the
  sandbox blocks the SBX daemon socket and reports it as stopped.

## Roll back

Every deploy keeps the previous unpacked releases:

```sh
ls -t ~/.warden/releases/
~/.warden/release/bin/warden stop
ln -sfn ~/.warden/releases/<older-directory> ~/.warden/release
~/.warden/release/bin/warden start --detach
```

Say which revision you rolled back to; another session may have expected
the newer one.

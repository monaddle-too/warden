# Claude provider on the OVH Warden deployment

## Objective

New chats on https://warden.monaddle.com can select Claude, but every Claude
run failed with "Claude runtime is not configured". The Claude integration
(`docs/warden-claude-provider-plan.md`) was only ever wired into the local
launcher; the OVH runner had no `--claude-path`, no Linux x86_64 Claude
executable existed on the server, and no Claude credential file was installed.
Deployments did not remove anything: it was never there.

## Decisions

- Pin the same Claude Code version the local integration was verified with
  (2.1.272), as the official `linux-x64` build, checksum-verified against
  Anthropic's release manifest. It lives outside the image at
  `/opt/warden/claude/claude` like the Codex runtime, so images stay
  provider-agnostic and the binary is not rebuilt into every release.
- The credential is a host-owned `claude setup-token` value in
  `/var/lib/warden/provider/claude.json` (0600, uid warden), mounted only into
  the policy container. Guests receive the usual broker placeholder. The token
  is entered by the owner through `scripts/warden-claude-token` at a hidden
  prompt; it never passes through chat, arguments or shell history.
- The policy broker reads the file lazily, so the stack starts and Codex chats
  keep working before the token is installed.
- Ship as a normal release (source + unchanged binaries, new image tag) and
  recreate all three containers so runner, policy and chat share one revision.
- (2026-09-15) Claude's built-in tools (Bash, Edit, Write, and the rest) are
  allowed without an owner prompt, matching Codex's `dangerFullAccess`. The
  SBX guest is the security boundary, egress goes through the Warden proxy,
  and each Warden MCP tool obtains owner approval itself, so the per-call
  `can_use_tool` prompt was a second, redundant approval on every command.
  Only `AskUserQuestion` still reaches the owner, as a question card.

## Steps

- [x] Confirm cause: runner args on OVH, no Claude binary, no credential file.
- [x] Install and verify `/opt/warden/claude/claude` 2.1.272 linux-x64.
- [x] Compose: `--claude-path` + read-only mount on the runner,
      `--claude-auth-file` on the policy broker.
- [x] Add `scripts/warden-claude-token` and document the flow in the README.
- [x] Stage and switch the release; recreate policy, runner and chat.
- [x] Owner installs the token; verify a real Claude chat on OVH.
- [ ] Record outcome here and in the workspace `WARDEN-OVH.md`.

## Deployment (2026-09-15)

- `/opt/warden/claude/claude` is the official 2.1.272 `linux-x64` build,
  sha256 `d81396a6…5bcd4` matching Anthropic's manifest; `--version` prints
  2.1.272; readable by uid warden. The local ARM64 binary the integration was
  verified with is the same version's `linux-arm64` build (checksum matched).
- Release `/opt/warden/releases/76dabb1`: source archive, `dist/ovh` binaries
  and `chat/web/dist` copied from 141ff80 (no Go or frontend source changed),
  `.env` copied with `WARDEN_IMAGE=warden:76dabb1`. Image built on OVH.
- Switched ~19:45 UTC with no active sandboxes or live publications:
  `/opt/warden/current` → 76dabb1, `compose up -d` recreated policy, runner
  and chat on `warden:76dabb1`. Runner args now carry
  `--claude-path /opt/warden-claude/claude` with the read-only mount; policy
  carries `--claude-auth-file /run/provider/claude.json`. Edge untouched.
- Post-deploy: root 200, signed-out `/api/state` 401, `warden-edge` and
  `warden-sbx` active, no errors in the three container logs. The token helper
  was self-tested as uid warden with a fake value into `provider/tmp` and
  removed; `/var/lib/warden/provider` still has no `claude.json`.
- Rollback: `ln -sfn /opt/warden/releases/141ff80 /opt/warden/current` and
  `compose up -d` there (runner/policy return to 8b01191 args without Claude).

## Remaining work

- Owner runs `claude setup-token` on a machine with a browser, then on OVH
  `sudo -u warden /opt/warden/current/scripts/warden-claude-token /var/lib/warden/provider/claude.json`.
- Build and release the built-in-tool auto-allow change (the deployed
  release 76dabb1 still prompts for every Bash/Edit/Write call).
- Verify a real Claude chat on warden.monaddle.com (model switch, no prompt
  for shell commands, preview tool approval) and note the result here. Token renewal is manual; the file
  records a 364-day expiry.

## Token install and first Claude chat (2026-09-15)

- The first token paste into the hidden prompt arrived truncated (81 of 108
  characters) and Anthropic answered 401 "OAuth access token is invalid".
  Piping the clipboard over SSH into the helper installed the full value; the
  owner's Claude chat on warden.monaddle.com then answered normally.
- The frontend shows "Message queued" while a run is being prepared, which
  looked like a hang because preparation was slow (below).

## Per-turn latency fix (release 2eac5c8)

- Measured on OVH: a trivial second turn took 13 s. `sbx cp` of the 227 MB
  Claude executable into the guest takes 7.6 s, and the runner did it at the
  start of every run (every message sent to an idle chat). Exec round-trip is
  0.4 s and CLI startup 0.7 s; the first turn additionally boots the guest and
  installs the Codex bundle (32 s observed).
- Fix: `managedSandbox.ClaudeInstalled` records a size/mtime fingerprint of
  the host executable once it is copied; later runs only check `test -x` in
  the guest and copy again when the host file changes or the guest copy is
  gone. Regression test `TestClaudeRuntimeCopiedOncePerGuest` covers repeat
  runs, worker restart, replaced binary and missing guest copy. `go vet` and
  the full race suite pass.
- Release 2eac5c8: Linux binaries rebuilt from this branch, `chat/web/dist`
  reused from 141ff80, image built on OVH, switched after waiting for the
  owner's running chat to finish. All three containers on `warden:2eac5c8`;
  runner still carries `--claude-path`. Root 200, signed-out `/api/state` 401,
  edge and SBX active. Rollback: 76dabb1 symlink and `compose up -d` there.
- Existing guests already hold `/tmp/warden-claude`; their first run on this
  release copies once more (no fingerprint recorded yet) and later runs skip it.

## Remaining work

- Confirm the improved turn time from the owner's next Claude messages.
- Token renewal is manual (364-day recorded expiry); the helper does not
  validate the token against Anthropic before writing it.

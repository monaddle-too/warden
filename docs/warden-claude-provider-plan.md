# Warden Claude integration (local)

Objective: run Claude through Warden locally and choose provider/model per conversation.

## Decisions

- Preserve the OVH deployment; use separate local state at `~/.warden-claude-local`, listening on `http://127.0.0.1:18781`.
- Keep actual provider credentials in the host broker. The sandbox receives a placeholder and Warden injects authentication only for the authorized provider routes.
- Existing chats default to Codex. Choose Codex or Claude when creating a chat. Provider choice is fixed once messages exist; model changes are allowed while idle and preserve provider history. Claude aliases include `sonnet`, `opus`, and `haiku`.
- Claude runs as its native Linux CLI inside SBX, with streaming, tool approvals, questions, history and Warden MCP tools translated into the existing conversation protocol. Messages arriving during a Claude turn queue for the next turn.
- SBX's own login writes a broker placeholder in the guest, not a transferable credential. Use a host Claude `setup-token` browser authorization instead. The local host credential file is private (mode 600), with expiry checking; no automatic renewal is implemented. The configured inference token is conservatively dated to expire after 364 days.

## Progress

- [x] Inspect Claude protocol and credential proxy requirements.
- [x] Add provider/model persistence, validation, API and UI controls.
- [x] Add Claude runner, streaming/history, approvals and model routing.
- [x] Verify Go race tests and vet, all 309 Python tests, and frontend production build.
- [x] Real local Claude and Codex conversations succeeded in a shared sandbox. Claude resumed history after switching Sonnet to Haiku and completed an approved file write. Its Warden MCP preview tool successfully attached port 3000.
- [x] Fix composer textarea width: browser-default intrinsic width left most of the box unusable. A full-width block now measures 943px inside a 945px bordered composer, verified in Chrome.
- [x] Preserve committed source and stable local runtime. Clean worktree can now be removed; branch retains all changes.

## Local operation

The launcher accepts `--claude-path` (native Linux ARM64 executable) and `--claude-auth-file` (host-owned JSON containing `claudeAiOauth.accessToken` and `expiresAt` in milliseconds), alongside the existing Codex runtime/auth options. Do not copy a sandbox's proxy placeholder into that file. Browser authorization is required when establishing or renewing the host token.

Local runtime/source export: `/Users/danielporter/Documents/warden-workspace/.local/warden-claude-source`. Launch instructions are in `LOCAL.md` and `start-local.sh` alongside that export. The stable local launcher PID is recorded in `~/.warden-claude-local/launcher.pid`. Verified full-width typing against this exported build; no real Claude token was found in app, runner or broker persisted state outside its private credential file. This is local only; production remains unchanged.

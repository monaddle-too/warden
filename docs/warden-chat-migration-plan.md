# Warden chat migration

## Objective
Move Panta’s chat UI and backend into Warden. Warden owns chat history, agent execution, and SBX lifecycle independently of Panta. Panta is a future integration only.

## Source and decisions
- Source: Panta commit bf61d5b072b024e3b5ad0b7a315c3610c2a4c828.
- Reuse its agent RPC transport, transcript reducer, managed SBX worker and tests, and conversation presentation.
- Add a Warden-owned local authenticated chat API and durable store. No Panta database, accounts, document instructions or running service required.
- Preserve existing Warden integrations and enforcement. Agent execution must fail closed when SBX/Warden are unavailable.
- Local single-owner deployment first; no migration of existing Panta history.

## Steps and progress
- [x] Inspect source and create dedicated codex/warden-chat worktree.
- [x] Extract backend and build standalone Warden chat API.
- [x] Adapt chat components and connect Warden API.
- [x] Verify persistence, streaming, stop/resume, steering, approvals and isolation.
- [x] Run backend race tests, vet, frontend checks and live SBX acceptance.
- [x] Document startup, limitations and source provenance; preserve implementation on the dedicated branch.

## Remaining work
Chat extraction is implemented. Full MCP product work, the unified connection/permission console, and Panta integration remain separate platform-plan steps. No Panta deployment or existing provider process was modified.

## Validation evidence (2026-09-14)
- Go backend, agent and sandbox tests pass with `-race`; `go vet ./...` passes.
- Browser retry identity tests pass; TypeScript and production Vite build pass.
- All 154 existing Python Warden regression tests pass.
- Real SBX 0.42.1 / Codex 0.154.0: created and reread `warden-chat-check.txt` containing `WARDEN_CHAT_OK` inside `/home/agent/workspace`.
- Restarted the standalone stack and resumed the same provider thread with its sandbox file retained.
- Explicitly shared that environment with a second chat; histories stayed separate.
- Agent attached a counter preview; the browser rendered its iframe and Increment changed 0 to 1. Detached preview processes survive turn completion; foreground agent command sessions do not.
- Live Stop initially exposed an ordering bug absent from the fake worker. Added cancellation-before-stop and bounded cleanup waiting plus a regression test. Retest interrupted a real sleep command, verified environment state `stopped`, and resumed the same chat, reread the saved file, and reattached the detached preview.
- Browser UI visually inspected. Permission/RPC approvals, request authentication, denial, uncertain delivery and wrong-sandbox rejection covered by automated tests; the general provider MCP/approval flow is not claimed here.

## Historical local acceptance installation
The local acceptance stack uses `~/.warden-chats` for private history, endpoint capability, worker registrations and logs. The web interface listens on `127.0.0.1:18780`. Source changes are preserved on `codex/warden-chat`. See `docs/warden-chat.md` for startup and scope. No existing Panta history was imported.

The active deployment is now OVH-only. See [authenticated previews and server deployment](warden-public-previews-plan.md); the Mac acceptance stack is stopped and its private data preserved.

# Google Docs integration plan

## Objective
Extend Warden's existing GitHub/Figma HTTP permission proxy to read Google Docs through a host-held OAuth connection, then connect the owner's account and verify a real approved document read.

## Decisions
- Dedicated branch codex/google-docs-integration, based on the completed Figma branch; preserve unrelated main-checkout work.
- Initial scope is read-only Docs API, with per-document/per-operation Warden approval. No document edits, broad Drive access, or new MCP transport.
- Use Google OAuth and memory-only credentials consistent with the existing prototype. Do not reuse Codex connector credentials.
- Preserve the running Figma connection; use a separate local control instance for Google acceptance if upgrading would lose its memory-only secrets.

## Steps
1. Done: inspect existing implementation, confirm Google API and OAuth requirements, inspect account setup.
2. Done: implement document-read adapter, OAuth, provider isolation, setup UI, and meaningful tests.
3. Done: configure a Google Cloud OAuth app, connect the owner's account, and verify denied/approved/replayed reads.
4. Done: document results and limitations, commit changes, preserve the runtime, and clean up the worktree.

## Remaining work
No remaining inputs for this read-only local integration. See limitations below.

## Completion checkpoint
- User selected adding a Warden client to Monaddle. Created Warden Google Docs
  OAuth web client, enabled Docs API, and connected the owner's Google account.
- User clarified no pasted OAuth grants for end users. Confirmed operator client
  setup is one-time; users receive automatic callback/token exchange after consent.
- Implemented scoped Docs GET adapter, memory-only PKCE OAuth and refresh,
  independent grants/allowlist, authenticated configuration, UI, and proxy routing.
- Live callback included Google's iss parameter. Added issuer validation and tests.
- Live HTTP/2 exposed duplicate Host/authority handling; used protocol-aware setter
  and verified approved upstream read with default curl after the fix.
- Acceptance passed: 428 before approval, 200 after exact grant, replay/other-doc
  428, write 403. No document changes; grants revoked and test requests cleared.
- Separate Google control plane on port 18772 preserves the original Figma session.
- Final verification: 137 tests passed; JavaScript syntax and git whitespace checks passed.
  Source/docs committed; preserve exported runtime at the same path during worktree cleanup. No public app
  publication, MCP transport, shared SBX onboarding, or durable secret store claimed.

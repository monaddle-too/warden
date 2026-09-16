# Provider model dropdowns

Objective: replace free-text model fields with hardcoded provider-specific dropdowns, in both new-chat setup and existing conversations.

Implemented: shared ModelSelect; Codex visible model IDs from installed Codex models_cache.json (gpt-6-astra, gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna, gpt-5.5); Claude existing supported aliases sonnet/opus/haiku. Provider default remains explicit; legacy saved custom model preserved as a current-only option. Provider switches reset model; running chats disable editing. No dynamic discovery requested.

Remaining: build, deploy idle chat service on OVH, inspect dropdowns, preserve and clean worktree.

Completed:78c575f deployed as chat image on OVH, with runner/policy/edge unchanged. Production build/typecheck passed and authenticated owner API is healthy. Browser currently requires sign-in, so visual verification was limited to loading deployed sign-in page; no account login or model execution was performed for this UI-only change. Hardcoded IDs are drawn from local installed model metadata, not a promise of every account's entitlement. Changes pushed origin/codex/warden-model-dropdown; source snapshot workspace .local/warden-model-dropdown-source preserved before clean worktree removal.

Follow-up: 78c575f had diverged from the Google login branch; both were merged as b40b973 and deployed together (see google-login-publishing-plan.md).

# Local Claude SBX trial

Objective: try Claude in a local Mac SBX sandbox, independently of OVH Warden.

- [x] Inspect local SBX; CLI available, no host Claude executable found.
- [x] Create a dedicated Claude sandbox without host workspace mounts or shared skills.
- [x] Try a minimal prompt and record any authentication requirement.
- [x] Preserve results and stop the trial sandbox; clean the worktree.

Use branch codex/claude-local-sbx-probe. Existing sandboxes and OVH remain untouched.

## Result
Success on 2026-09-14: local sandbox `warden-claude-probe`, 1 CPU and 2048 MiB,
Claude Code 2.1.246 using the built-in Claude image. Authenticated through the
standard browser OAuth flow with the owner's Claude Pro account; no PAT or
API key copied from the host. Initial unauthenticated trial correctly failed.

Executed `sbx exec warden-claude-probe claude -p 'Reply with exactly CLAUDE_SBX_OK. Do not use tools.'`
and received `CLAUDE_SBX_OK`, exit 0. This was a model inference smoke test,
not a Warden provider integration or a file-editing benchmark.

Stopped the sandbox afterward, preserving its login for further local trials.
Resume with `sbx run --name warden-claude-probe`.

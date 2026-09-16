# Conversation repository sharing

Objective: let the owner select GitHub repositories for persistent, read-only conversation access.

Decisions: use the connected GitHub App account (this local Warden is single-owner); list owned repositories available to that installation, including private repositories. GitHub installation selection remains the upper permission boundary. No PATs or user tokens. Selections persist until removed, scoped to conversation and sandbox. Reuse strict REST/Git operation parsing and inject short-lived, repository-scoped installation credentials only on authorized reads. Agents discover URLs through list_shared_repositories.

Steps:
- [x] Extend private broker with paginated repository discovery.
- [x] Persist selections and enforce read-only proxy authorization/revocation.
- [x] Add conversation selector and agent discovery tool.
- [x] Test boundaries, build, verify live broker and local UI, preserve changes.

Progress: inspected existing Google sharing, trusted sandbox leases, and GitHub installation broker. Existing configured account is monaddle-too; discovery must verify that account against GitHub's installation response. Repositories outside the app installation require installation configuration before sharing.

Implementation completed: private metadata-only paginated broker discovery; durable SQLite grants bound to account/app/repository ID/conversation/sandbox; strict read-only proxy matching and in-flight revocation; owner-only selector API; agent list_shared_repositories tool for Claude and Codex. The selector retains existing selections when discovery fails so access can be removed offline. Saving refreshes GitHub metadata; a repository transferred/deleted/recreated under the same name cannot inherit access.

Validation so far: 318 Python tests passed before the final additional discovery/proxy cases; targeted broker/grant tests pass; actual mitmproxy injection and in-flight revocation pass. All Go race tests and go vet pass; frontend production build passes. Live broker verified monaddle-too user installation 160513175 and private panta repository 1358422429. New broker installed alongside the old broker at /opt/warden-github-broker/github_sharing.py; old service/config untouched. Local sharing-broker.json points to the new broker. Remaining: local UI and live authenticated read acceptance, preservation/cleanup.

GitHub discovery semantics: https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation . This single-owner local app uses the connected installation account; adding general multi-user GitHub sign-in is outside this change.


Acceptance complete: system Chrome displayed the verified private panta repository; selection saved and remained checked when reopened. Live Claude list_shared_repositories returned the exact repository, clone/API URLs and null expiry. Claude then ran Python urllib.request inside the managed sandbox with default TLS and no Authorization header: HTTP 200, full_name monaddle-too/panta. Temporary selection removed through the UI after testing. The full Python suite passed (318 cases), then all 42 targeted broker/grant/proxy cases passed including the added discovery and in-flight revocation tests. Go race suite/vet and production frontend build passed.

Local runtime source preserved outside worktree at /Users/danielporter/Documents/warden-workspace/.local/warden-github-sharing-source, using start-local.sh and private sharing-broker.json. Local URL remains http://127.0.0.1:18781. OVH Warden production was not deployed; only a separate version of the existing private broker was added for local development. No user action is pending.

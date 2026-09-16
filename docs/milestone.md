# Protected development milestone

Original eight-point scope, September 9, 2026. Completion is measured by working
code and validation; external prerequisites and unresolved findings stay explicit.

1. Repository workflow: existing clone/build/check/review/approved branch/PR path;
   preserve separate Git and REST operation permissions.
2. Egress: exact destination/method profiles for AI, dependencies and development;
   enforce HTTPS and guest DNS, including Apple TLS exceptions.
3. Project isolation: independent VM state, policy, repository allowlist and keys;
   fresh provisioning without copying personal credentials between projects.
4. Recovery: stopped-disk checkpoints, verified restore, disposable project state;
   keep current policy/audit and invalidate authorizations after restoration.
5. Menu: existing health/approvals/SIEM plus revoke-all and network disconnect,
   with explicit requested versus applied state.
6. Notifications: grouped, deduplicated approval notices opening the exact request;
   no automatic approvals, no request bodies on the lock screen.
7. Installation/update: readiness wizard, release manifest/signing/notarization
   recipe, verified update staging/rollback, reversible uninstall.
8. Validation: automated policy/host/proxy regression and failure injection,
   recovery/update fault tests, live guest network and storage/kernel diagnostics.

External prerequisites: no Developer ID signing identity is installed on this
Mac. Apple signing/notarization cannot complete until one is supplied. Historic
kernel/storage anomalies cannot be called resolved merely by passing short tests.

## Implementation status

| Original item | Result |
| --- | --- |
| Repository workflow | Existing reviewed branch/PR workflow retained; operation and repository gates continue to apply. |
| Code egress policy | Implemented and active: exact host/method profiles, DNS gate, Apple policy gate and persistent cutoff. |
| Project isolation | Implemented: separate fresh VM states, per-project keys/policy/credentials and repository allowlists; one active desktop controller at a time. |
| Recovery | Implemented and tested: stopped-disk capture/checksums, staged/journaled restore and rollback; grants never resurrect. Off-device backup needs a destination. |
| Menu controls | Live: health, backlog, approvals, revoke-all, acknowledged disconnect/reconnect. |
| Notifications | Live and delivery verified: grouping, global/per-group rate limits, persistent deduplication and exact review links. |
| Installation and updates | Implemented/tested: readiness guide, locally signed manifest, pinned verification, managed installation, update/rollback and reversible uninstall. Apple signing/notarization awaits Developer ID credentials. |
| Security validation | 135 Python tests, native checks, 10 actual network checks, controller/proxy failure, real audit ENOSPC, bounded memory/storage tests. Historical kernel/storage fault still unresolved. |

See [the operating guide](protected-development.md) and [validation evidence](validation.md).
These results do not constitute a claim that the prototype is production-certified.

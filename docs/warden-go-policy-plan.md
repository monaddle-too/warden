# Go policy broker for the OVH deployment

## Objective

Replace the Python machinery that runs in the OVH `policy` container with Go:
`python -m warden.sbx` (registry, control socket, per-sandbox policy engines,
SBX verifier, credential sources, sharing, images, pull requests) and the
per-sandbox `mitmdump` gateways driven by `proxy/sbx_addon.py`. After this
change the Warden image contains no Python or mitmproxy; the three Compose
services are `warden-policy`, `warden-runner` and `warden-chat`.

The runner and chat binaries keep speaking the existing version 1 control
protocol over `sbx-control.sock` (`register`, `check`, `begin`, `renew`,
`end`, `gateway`, `configureProvider`, `bindGateway`, `proxy`, `sharing`),
so they are unchanged. Production state under `/var/lib/warden/policy`
keeps its layout (`bindings.json`, `runtime-identities.json`,
`sandboxes/<digest>/{policy.json,control.sqlite,audit/events.jsonl,
gateway-port.json,gateway/ca}`, `sharing.sqlite`, `google.sqlite`) so the
existing grants, blocked documents, repository selections, images, pull
request proposals and the Google connection carry over, and rolling back to
the Python image remains possible.

## Decisions

- One binary, `chat/cmd/warden-policy`, with the SBX service as its default
  command and `github-broker` as a subcommand replacing
  `warden.github_app` (`--store`, `--app-id`, `--owner`, same stdin/stdout
  JSON protocol). `/var/lib/warden/github/broker.json` on OVH must point its
  `command` at `/usr/local/bin/warden-policy github-broker …` at deploy time.
- SQLite through `modernc.org/sqlite` (pure Go, cross-compiles from the Mac
  without a C toolchain), same schemas and files as the Python code.
- Gateways run in-process: one loopback listener per registered binding
  instead of a `mitmdump` child. The `/__warden_sbx_health/<nonce>` HMAC
  probe stays, and the verifier still requires a live probe before every
  proof, so a dead listener fails closed exactly as before.
- An existing `gateway/ca/mitmproxy-ca.pem` is reused so resident guests
  keep trusting the gateway across the switch; otherwise a Warden CA is
  generated and written to the same file names.
- Guest-facing TLS negotiates HTTP/1.1 only; upstream connections use a
  freshly resolved, validated, pinned IPv4 address with SNI equal to the
  request authority, system roots, no redirects and no compression
  negotiation on Warden's behalf. CONNECT is inspected; raw TCP, WebSocket
  upgrades and TLS passthrough are refused as in `sbx_addon.py`.
- Provider SSE responses are streamed through the same bounded inspector
  (identity/gzip/deflate, 16 MiB, secret suffix retention); other responses
  are buffered (16 MiB, 128 MiB for Git) and audited before delivery.
- Ported although not configured on OVH, because the gateway code paths
  share them: the local document API dispatch (`--document-api-origin`,
  `--document-api-key-file`) and Figma request classification (denied with
  503 because this deployment has no Figma connection API).
- Out of scope and still Python: the macOS VM appliance (`proxy/addon.py`
  transparent mode, nftables, Apple update passthrough, clipboard, SIEM,
  dashboard HTTP UI, retention/OCSF tooling, `scripts/warden-chat`
  launcher on the Mac). `scripts/warden-chat` is updated to start the Go
  binary instead of `python -m warden.sbx`.

## Steps

- [x] Port canonical JSON, redaction, hash-chained audit, egress policy and
      the Git smart-HTTP grammar.
- [x] Port the per-sandbox policy engine (SQLite requests/grants/decisions,
      GitHub REST catalog, normalization, approvals, egress decisions, Git
      and pull-request reviews) and the retention scrub.
- [x] Port credential sources (Codex, Claude), the GitHub App credential
      client and store broker, Google Docs/Figma adapters and document
      writes.
- [x] Port sharing (Google grants, blocked documents, repository selection,
      images, pull request proposals and publication).
- [x] Port the registry, control protocol and Unix socket server.
- [x] Port the SBX CLI verifier and the in-process gateway (CA, CONNECT
      interception, reverse provider/document routes, authorization, stream
      inspection, revocation watcher, audit).
- [x] Port the Python test suites that cover the OVH path to Go tests; run
      the race suite and vet.
- [x] Replace the image (`deploy/chat/Dockerfile`, `compose.yaml`), update
      the launcher and README, record the deployment procedure.
- [x] Build the image on OVH, stage a release, check `broker.json`, switch
      the policy container and run the live acceptance below.

## Deployment procedure

1. Build the four Linux binaries and the frontend, upload the release
   (`config/policy.template.json`, `vendor`, `deploy/chat`, `dist/ovh`,
   `chat/web/dist`), and `docker build -f deploy/chat/Dockerfile -t
   warden:<revision> .` on OVH. The build fails if `warden-policy
   selfcheck` cannot load the installed layout.
2. Inspect `/var/lib/warden/github/broker.json`. If its `command` invokes
   Python inside the container, change it to
   `["/usr/local/bin/warden-policy","github-broker","--store",…,"--app-id",…,"--owner",…]`.
3. With no active sandboxes or published previews, point
   `/opt/warden/current` at the release, update `.env`'s `WARDEN_IMAGE`, and
   `docker compose up -d` to recreate policy, runner and chat. The edge is
   unchanged.
4. Acceptance: `docker logs warden-policy-1` prints "SBX control ready";
   a Codex chat and a Claude chat each complete a turn (provider route,
   credential injection, SSE streaming); a shared GitHub repository read
   and a shared Google document read succeed; an environment stop revokes
   in-flight access; a published preview survives the switch. Check the
   sandbox audit files under `/var/lib/warden/policy/sandboxes/*/audit`
   for `sbx.run.started`, `sbx.provider.authorized` and `http.response`.
5. Rollback: switch `/opt/warden/current` and `WARDEN_IMAGE` back to the
   previous release and recreate the containers; state needs no migration.

## Progress

- 2026-09-15: inventory complete. The OVH `policy` service runs
  `warden.sbx` with `--sbx --mitmdump --manage-network --codex-auth-file
  --claude-auth-file --google-config` and `WARDEN_GITHUB_APP_BROKER`; no
  document API is configured there. The Go module proxy is unreachable from
  this development sandbox, but `modernc.org/sqlite v1.58.0` and its
  dependencies are already in the local module cache (Panta uses the same
  version), so the dependency is added with those exact versions.
- 2026-09-15: merged main (0b1d270, resident Claude sessions). The Python
  change to the verifier (background proof refresher every 12 s for warm
  bindings, `last_begin` per binding, fresh proofs served without CLI
  inspection after a live gateway probe, stop on close) is ported to
  `SbxCliVerifier` with explicit locks: CLI inspection is serialised by a
  verifier mutex, the registry lock is only taken to snapshot the binding,
  and the proof cache has its own mutex. `RefresherTests` are ported.
  `go vet` and `go test -race ./...` pass; merged into main.

## Deployment (2026-09-15, release fcf98b6)

- Staged `/opt/warden/releases/fcf98b6` (source tree, `dist/ovh` binaries
  built from fcf98b6 with `-X main.revision`, `chat/web/dist` copied from
  0b1d270, which has no frontend change). Image `warden:fcf98b6` built on
  OVH (319 MB, down from 536 MB); `warden-policy selfcheck` and `git` run
  inside it. Private pre-switch backup of `policy`, `github` and `app` state
  plus the old `broker.json` at `/var/backups/warden/go-fcf98b6/`.
- `/var/lib/warden/github/broker.json` did run `python -m warden.github_app`
  inside the container; its command now is
  `/usr/local/bin/warden-policy github-broker --store /run/github --app-id
  4893871 --owner punished-monaddle`.
- Switched about 21:48 UTC with no running chat and one stopped sandbox;
  policy, runner and chat recreated on `warden:fcf98b6`. Policy logged
  "SBX control ready", edge 200, sharing status unchanged (Google connected
  with write scope, GitHub connected).
- Acceptance through the local chat API: GitHub repository discovery through
  the Go broker subcommand (installation 160513175, two repositories) and
  Google Drive listing both 200. A new Claude chat and a new Codex chat each
  answered "PONG" (Claude 7 s, Codex 7 s from run start to first response).
  Sandbox audits show `sbx.run.started`, `sbx.provider.authorized`
  (api.anthropic.com / chatgpt.com), `http.response` with redacted headers,
  Codex SSE via `http.response.started`, lease renewals, and no unredacted
  bearer tokens. New sandboxes received a generated Warden CA under the
  mitmproxy file names. A shared repository was fetched by Claude with curl
  through the gateway (`sbx.github.authorized`, installation token injected,
  answer `punished-monaddle/cf-docs` in 6 s), and a second resident turn
  answered in 2 s. Test chats were stopped and archived; the repository
  share was removed.
## Verifier off the chat path (2026-09-15)

Owner decisions: the policy verifier must not run on the startup path of a
chat, and its repeat interval is two minutes, not twenty seconds.

- Host-wide checks (pinned daemon version, `ssh.agentForwardingEnabled`,
  `proxy.sandbox`, empty MCP inventory, no global network rules, implicit
  global denial) are one cached fact for the whole host, valid for the proof
  lifetime and refreshed by the background loop on its own, so neither a
  second phase nor a second sandbox re-runs them.
- Per-sandbox checks (runtime identity and image pin, the sandbox-scoped
  gateway-only rule, the probe checks) run once when a runtime is first
  proven and then only by the refresher while the binding is warm.
- Timings: proof lifetime 300 s, refresh every 120 s (host first, then every
  warm binding), warm window 1200 s after the last `begin` so a resident
  sandbox stays covered longer than the runner's 15-minute idle stop. The
  refresher also runs once at service start, so the first chat finds the
  host already proven. The registry accepts proofs up to 300 s ahead.
- The chat path (`check`, `begin`, `renew`) only probes the gateway and reads
  the cached proof. A failed host check clears every proof and applies the
  deny rule to warm sandboxes; a failed per-sandbox check behaves as before.
- Owner decision (later the same day): `begin` is not gated on a first
  successful inspection either. A binding without a fresh proof gets a
  provisional proof as soon as its gateway is up and its gateway-only network
  rule is in place (the sandbox is created by the trusted runner with the
  bootstrap deny rule, and moving it to the single gateway allow rule is
  configuration the broker owns, two or three CLI calls, not evidence). The
  full host and sandbox verification then runs in the background right away
  and every two minutes while the binding is warm. What is assumed until
  that first background pass finishes (a few seconds): the pinned daemon
  version and settings, the empty MCP inventory, the absence of global
  rules, and the runtime's identity and image pin. A failed verification
  clears every proof and applies the deny rule, so the run is revoked at its
  next lease renewal or control call, and a binding that failed once is
  verified synchronously until a full pass restores trust. A gateway that
  does not answer its probe, or a sandbox whose rules cannot be brought to
  the gateway-only state, still fails closed immediately.
- Deployed 2026-09-15 as release 1268343. A new Claude chat's first turn
  took 26.7 s (27.1 s on ea1d822), its run started 20 s after the message
  and answered 7 s later; the background verification passed and a second
  turn after 30 s took 3.1 s. So the synchronous inspection was not what
  dominated the first message: the 20 s before `sbx.run.started` is
  sandbox creation and guest preparation in the runner (VM create and
  boot, CA and Claude executable copies, process spawn), which the policy
  service does not see. Measuring those steps needs runner-side timing.
- Not changed here: the runner does not yet overlap guest preparation with
  verification. (The per-run CA install was removed separately by the
  host-wide gateway CA change, `docs/warden-gateway-ca-plan.md`.)
- Deployed 2026-09-15 as release e1c35da, superseded within minutes by
  ea1d822 (the host-wide CA release, which merges e1c35da). Measured on
  ea1d822 through the chat API with a new Claude chat: first turn on a new
  sandbox 27.1 s; a turn after 330 s idle (past the 300 s proof lifetime)
  2.6 s; a turn after a further 130 s idle 2.1 s. Before this change a
  turn after more than 20 s idle re-ran the 5 s host inspection.

- Rollback: restore `/var/backups/warden/go-fcf98b6/broker.json.python` to
  `/var/lib/warden/github/broker.json`, `ln -sfn
  /opt/warden/releases/0b1d270 /opt/warden/current` and `docker compose up
  -d` there. State needs no migration.

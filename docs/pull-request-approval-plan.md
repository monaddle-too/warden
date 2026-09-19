# Pull request proposals and owner approval

Objective: agents submit complete proposed PRs; Warden displays a GitHub-style title, description, base/head and file diff; only explicit owner approval publishes; rejection returns feedback so the agent can revise.

Design: request_pull_request takes repository, base branch, title, body, and complete UTF-8 contents for each changed file (null deletes). Host reads the immutable base commit, builds the diff itself, and stores an immutable proposal. Read access to the repository is required. Approval creates a dedicated warden/pr-<proposal> branch and PR from exactly that stored snapshot, never the agent's mutable working directory. Reject binary/symlink/submodule/mode changes in this first version. Base changes or revoked sharing require a fresh proposal. No broad write credentials go to the sandbox. Publication is asynchronous and durable; repeated approval cannot duplicate publication. Partial failures are explicit and show the branch for recovery, with no automatic retry after ambiguous GitHub responses.

- [x] Durable proposal validation, trusted preview and publication service.
- [x] Agent waiting tool and durable resolution delivery.
- [x] GitHub-style review dialog, approve/reject with feedback.
- [x] Security/failure tests, builds, local acceptance and cleanup.

No real PR will be published as a test without approval of its concrete contents. Use simulated GitHub publication tests and live read-only proposal/rejection acceptance.


User refinement: Warden performs submission itself. The proposal must include the PR body, and the body is editable by the owner in the review UI. Approval submits the exact editor value, stored atomically with a SHA-256 digest before publication. Both publication startup and the final GitHub request enforce this digest; no agent-submitted replacement body can inherit approval. Rejection returns both feedback and any owner-edited body to the agent.

Acceptance criteria:
- The review screen shows the submitted body and lets the owner edit and preview Markdown.
- The body in the actual GitHub create-PR payload matches the owner's approved UTF-8 body byte for byte, including whitespace/newlines.
- Missing reviewed body or any changed body after approval blocks publication; mismatch tests assert no GitHub writes occur.
- Rejection performs no GitHub writes and resumes chat with feedback and suggested body edits.


Implementation complete. Validation: full Python suite 327 tests passed before the body-editor additions; all 10 proposal/publication tests now pass, covering editable body byte equality, mismatch rejection before writes, missing approval body, immutable files, stale base, revoked repository, idempotency, crash/ambiguous failure, and feedback delivery. Go race suite and vet passed; frontend production build passed. Next: verify live read-only proposal, body editing and rejection with Claude in system Chrome; preserve local source and clean worktree.


Acceptance completed locally with Claude and system Chrome. Claude submitted title/body/full text file through request_pull_request. Warden fetched main at f38fee2e7201 and displayed a +3-line diff. Edited the body in the review textarea and confirmed the rendered Markdown immediately matched. Rejected with feedback; the waiting tool returned rejected + edited_body + feedback. Claude acknowledged the exact edited body and returned to idle without resubmitting. No actual branch or PR was published. Temporary repository grant removed and acceptance conversation archived after verification.

All 11 targeted tests pass, including denying an agent's direct POST with an unreviewed body (the approval grants no sandbox write access). Earlier full Python suite: 327 passing; Go race suite/vet and latest chat race tests pass; frontend production build passes. Source/runtime preserved at /Users/danielporter/Documents/warden-workspace/.local/warden-pr-approval-source. The local service remains on http://127.0.0.1:18781. Production Warden was not deployed. No user action is pending.


## Updates and CI results (2026-09-19, docs/dogfood-loop-plan.md)

Two affordances the dogfood loop lacked: an agent could open a pull
request but not push a fix to it, and could not see whether it built.

- **Update a pull request.** `request_pull_request` with `pull_request: N`
  (and no `base`) proposes files against the open pull request's current
  head; Warden reads the pull request (`pulls/get`), accepts only a head
  branch it published itself (`warden/pr-…`, same repository), computes
  the diff from that head, and stores the snapshot as before. Approval
  (same review, same exact-body rule) creates the tree and a commit whose
  parent is the reviewed head and fast-forwards the branch with
  `git/update-ref` (`force: false`; a branch that moved is refused before
  and by GitHub). The commit message is the reviewed title and body; the
  pull request's description is not rewritten. No new branch, no new pull
  request; the result carries `number`, `url`, `commit`.
- **View CI results.** `view_ci_results {repository, pull_request | ref}` is
  a read, answered at once: the head commit's check runs
  (`checks/list-for-ref`), and for each failed GitHub Actions job its
  steps (`actions/get-job-for-workflow-run`) and the tail of its log with
  the `##[error]` lines (`actions/download-job-logs-for-workflow-run`,
  the redirect followed without the credential). Owner credential, no
  token to the agent, the repository must be selected for the chat. The
  answer says whether checks are pending, none, success or failure and
  what to do next.
- **CI on pull requests.** `.github/workflows/ci.yml` (gofmt, vet,
  `go test ./...`; web install, build, test) runs on every pull request on
  the Mac runner, so a proposal that does not build shows up as a failed
  check the agent can read.

# GitHub sign-in from the browser

## Objective

In user-token mode the GitHub sign-in could only be made or refreshed from a
terminal (`warden login github`); a rejected or expired token failed closed
in the UI with "Refresh the GitHub sign-in before using repositories" and no
way to do so. The owner can now sign in, and refresh a lapsed sign-in, from
the admin console and from the repository dialog.

## Shape

- The policy service runs GitHub's device flow, because it owns the
  credential: a private 0600 file on the sbx shapes
  (`providers.github.authFile`), a Secret through the credential store in
  the kubernetes kind (`providers.github.secret`). The chat service never
  sees the token; the browser only ever sees the user code and the login.
- `chat/internal/login` exposes the flow as `DeviceFlow` (`RequestCode`,
  `Wait`), shared by the CLI and the policy service; tests point it at a
  fake GitHub.
- `policy.GitHubSignIn` (`chat/internal/policy/githublogin.go`) holds one
  attempt at a time: `Start` requests a code and polls in the background,
  `Status` reports `none | pending | done | failed`, `Cancel` stops it. A
  second start while one is pending returns the same code (a reloaded
  page); a start after expiry or failure begins afresh.
- `GitHubUserCredentials.Save` writes the confirmed record where `read`
  looks (`FileCredentials.Store` or the credential store), atomically, and
  registers the token with the redactor. The next use picks it up; nothing
  restarts.
- Sharing operations, forwarded by the chat service:
  `github_login_start` (POST), `github_login_status` (GET),
  `github_login_cancel` (POST). All three are owner-only at the edge: the
  user code binds whichever GitHub account types it to this Warden.
- Completion: `github_signed_in` repository event `{login, previous?}`.
  A refresh as the same login keeps the repository selections (that is
  what a refresh is for); a different login deletes them, exactly as a
  disconnect does.
- UI: `GitHubSignIn.tsx`. Idle: "Sign in with GitHub" or "Refresh
  sign-in". Pending: the code (copied to the clipboard when the browser
  allows, with a Copy button), a primary link to the verification page,
  Cancel, the expiry time; polls status every 2 s. Done: "Signed in to
  GitHub as X" and the console reloads. The admin console shows it in the
  GitHub section (user mode); the repository dialog shows it for the owner
  when the sign-in is absent or the list failed with the refresh message.
- App-broker mode is unchanged: no sign-in of its own; the operations
  refuse.

## Also fixed

`Disconnect GitHub` was a silent no-op on the Kubernetes credential store
(`os.Remove("")`); it now stores an empty record, which fails closed like a
missing one.

## Verification

- Go: `policy/githublogin_test.go` (sign-in from an existing and from no
  token, another account drops selections, same account keeps them, the
  token never appears in status or history, refusal / cancel / expiry /
  no client ID / App broker, the credential store path and disconnect);
  `edge/edge_test.go` (owner-only). `login`, `chats`, `edge`, `cmd/warden`
  suites pass.
- Web: `pnpm build && pnpm test`.
- Live (2026-09-17, local install, user-token mode): "Refresh sign-in"
  obtained a real device code from GitHub, showed it with the link and
  expiry, polled status, and Cancel cleared it. Completing the
  authorisation at GitHub was left to the owner.

## Remaining

- Nothing planned. Possible later: a device-flow sign-in for the first
  install run (`warden install`) pointing at the console instead of the
  terminal.

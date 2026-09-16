# Live sandbox document acceptance — September 11, 2026

An actual Codex agent in SBX 0.42.1 completed the document workflow through production Warden enforcement. The disposable app used a new SQLite database, private signing key and local owner browser session; existing workspaces and sandboxes were not changed.

## Observed result

- Runtime: wc-24d60d2512f3cc9d3c8fee0a; sandbox 409FC116-5E45-458A-A824-5D911E3E9A60.
- Chat B228D976-E339-4CDF-B56C-9B95D9923A34; run 23F57160-EBAB-4935-A4BA-731207CAC307; provider thread 01a09180-c200-76e3-bd43-2ed7ba843d1f.
- Owner created document 9229B4C9-8D7C-4AB4-A8BF-7BCD0E017F2A at revision 1.
- The agent read WORKSPACE_DOCUMENT_API_URL, listed/read the document, then submitted proposal 864177dd-4776-4d06-ba0b-4be5b8b4444b. Warden audit recorded GET 200, GET 200, POST 201 with the correct project, sandbox, chat and run attribution.
- Agent proposed exactly the requested appended sentence, preserved the original content, and reported that owner acceptance was still required.
- The browser displayed the proposal while accepted content remained at revision 1. Clicking Accept suggestion created revision 2. The rendered document and reloaded editor both contained the accepted sentence.
- After the run ended, a request from the same sandbox to its document gateway returned HTTP 503. The idle timeout had stopped the sandbox; the explicit post-run probe restarted it without an active lease, and access remained denied. It was stopped again afterward.
- Exact-byte scan of 81 copied guest Codex-state files (4,262,395 bytes) found zero matches for the app owner token, document signing key or current host access/refresh/ID tokens. This check covers the copied guest state, not all guest memory or files. No secret values were printed or sent into the sandbox.

## Validation and preservation

No application-code changes were needed after the previously passing Go race suite, 32 web tests, 238 Warden tests, browser and controlled HTTP integration checks. The fetched origin/main tips were already ancestors of the local baselines, so this merge introduces no unrelated upstream changes. PostgreSQL integration remains unrun without WORKSPACE_TEST_POSTGRES_URL.

Test sandbox, app, worker and Warden are stopped. Runtime state, private SQLite database, audit records, copied guest state and screenshots are preserved under /Users/danielporter/Documents/warden-workspace/.local/dl. Do not commit that private directory. The source-only evidence here supports the authorized local-main merges; no remote push or deployment was requested.

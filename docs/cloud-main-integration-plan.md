# Cloud main integration

Integrate the deployed organization, authentication, PostgreSQL persistence, Panta bridge, external MCP, calm navigation, references, document deletion, device recordings and SMTP invitations into public main. Keep Panta implementation in its private repository.

Publish a scrubbed squash to avoid disclosing private identifiers from development history. Deployment itself is unchanged.

Validation: web build and 309 tests passed. PostgreSQL cloud/authentication/recording tests passed. Full Go suite encountered an existing resident-session timing test; isolated repetition passed 10 times. Remaining full-suite checks are recorded below when complete.

The live UI-generated device-key acceptance test remains pending user confirmation; this merge does not claim that test passed.

# Cloud main integration

Integrate the deployed organization, authentication, PostgreSQL persistence, Panta bridge, external MCP, calm navigation, references, document deletion, device recordings and SMTP invitations into public main. Keep Panta implementation in its private repository.

Publish a scrubbed squash to avoid disclosing private identifiers from development history. Deployment itself is unchanged.

Validation: web build and 309 tests passed. PostgreSQL cloud/authentication/recording tests passed. Full Go suite encountered an existing resident-session timing test; isolated repetition passed 10 times. The broad Go run also stalled in the Kubernetes worker lifecycle test and was stopped for diagnosis; that test passed in isolation on both the integration tree and existing main. The broad suite is not reported as fully green.

The live UI-generated device-key acceptance test remains pending user confirmation; this merge does not claim that test passed.

Panta private integration: Docs build and service tests (12 pass, 1 environment-dependent skip), backend race suite and vet, and web integration/build plus 73 tests passed. Its exact tested tree was published as a squash using the GitHub noreply identity; original local history retained.

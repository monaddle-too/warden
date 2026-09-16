# Third-party notices

The offline dictionary in `lib/catalog.json` derives from the Open Cybersecurity Schema Framework (OCSF) schema, version 1.6.0.

OCSF is maintained by the OCSF contributors. Licensed under Apache License 2.0; the upstream license is reproduced in `licenses/OCSF-LICENSE`.

Source: https://github.com/ocsf/ocsf-schema/tree/v1.6.0

The dictionary is a transformed subset produced by `scripts/update-schema.mjs`: class identifiers and captions, category identifiers and captions, and selected top-level attribute metadata. HTML markup in descriptions is stripped. The underlying OCSF schema is not modified.

Other dependencies retain their respective licenses in their npm packages. The application UI was scaffolded with the Sites starter and includes its shadcn/Base UI component primitives.

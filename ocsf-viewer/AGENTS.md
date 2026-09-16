# OCSF Explorer

Work within this folder; the parent repo contains unrelated work by other agents.
The only shared integration point is `.github/workflows/ocsf-deploy.yml`.

## Dependencies

Use the pinned Nix environment: `direnv allow` once, then work normally here.
For non-interactive commands use `nix develop . --command <command>`.
Nix and direnv must already be installed. The interactive shell needs its direnv
hook (for zsh: `eval "$(direnv hook zsh)"`). Do not install global npm tools or
silently fall back to a different Node version. Commit `flake.lock` changes.

Nix pins development/CI tools. `package-lock.json` pins JS dependencies;
install with `npm ci`. Docker base images are pinned by digest separately.
Docker Engine with the Compose plugin is a host prerequisite, not a Nix service.

## Validation and deployment

Run `npm run check` and `npm run build` before shipping. Test the built container
with `scripts/smoke.mjs`; `deploy/README.md` documents production and recovery.
CI on pull requests validates/builds; pushes to main deploy only this app to OVH.
Never commit secrets, production data, `.env.production`, or private SSH keys.
Never bundle unrelated parent-repo changes. Do not deploy from a workstation as
a substitute for checking the actual GitHub Actions deployment path.

The backend MUST be written in Go. React builds to static files served by Go.
The production target is an OVH VPS using Docker Compose. The retained
Sites metadata is scaffold history, not the production deployment target.
Preserve the local import/viewer workflow as server ingestion is added.

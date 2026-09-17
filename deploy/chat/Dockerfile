# Warden server image: the one warden binary (run as warden policy, warden
# runner and warden serve by compose.yaml) with the built UI, the GitHub catalog and the policy template at the /app paths
# warden.example.json names. Built for linux/amd64 and linux/arm64 by
# .github/workflows/release.yml from the cross-compiled binaries in
# dist/linux-<arch>/ (a local build needs the same layout: see
# deploy/chat/README.md, "Build and launch"). The base image is the
# multi-architecture index of debian:bookworm-slim pinned by digest
# (Docker Hub, 2026-09-16); buildx resolves the platform manifest.
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
# Set by buildx from --platform: amd64 or arm64. A plain `docker build` sets
# it to the host architecture.
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git tini && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY config/policy.template.json /app/config/policy.template.json
COPY vendor /app/vendor
COPY dist/linux-${TARGETARCH}/warden /usr/local/bin/
COPY chat/web/dist /app/web
RUN chmod -R a+rX /app /usr/local/bin/warden
# Packaging smoke check: the catalog, policy template and gateway CA
# generation must work from the installed layout before the image ships.
RUN warden policy selfcheck && git --version && warden --version
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]

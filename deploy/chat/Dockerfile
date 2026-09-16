# Warden server image: warden-policy, warden-runner and warden-chat with the
# built UI, the GitHub catalog and the policy template at the /app paths
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
COPY dist/linux-${TARGETARCH}/warden-policy dist/linux-${TARGETARCH}/warden-chat dist/linux-${TARGETARCH}/warden-runner /usr/local/bin/
COPY chat/web/dist /app/web
RUN chmod -R a+rX /app /usr/local/bin/warden-policy /usr/local/bin/warden-chat /usr/local/bin/warden-runner
# Packaging smoke check: the catalog, policy template and gateway CA
# generation must work from the installed layout before the image ships,
# and the three binaries must report one revision and protocol.
RUN warden-policy selfcheck && git --version \
    && warden-policy --version && warden-runner --version && warden-chat --version \
    && test "$(warden-policy --version | cut -d' ' -f2-)" = "$(warden-runner --version | cut -d' ' -f2-)" \
    && test "$(warden-policy --version | cut -d' ' -f2-)" = "$(warden-chat --version | cut -d' ' -f2-)"
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]

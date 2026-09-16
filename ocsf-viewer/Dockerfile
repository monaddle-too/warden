# Build artifacts with scripts/build.sh inside the pinned Nix shell first.
FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40
ARG REVISION=development
LABEL org.opencontainers.image.revision=$REVISION
WORKDIR /app
COPY --chmod=0555 build/ocsf /app/ocsf
COPY dist/client/ /app/public/
ENV PUBLIC_DIR=/app/public LISTEN_ADDR=:8080 APP_REVISION=$REVISION
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/ocsf"]

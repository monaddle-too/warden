#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
node scripts/sync-contract.mjs --check
npm run check
(cd backend && go test -race ./... && go vet ./...)
npm run build
npm run test:worker
test -s dist/client/index.html
mkdir -p build
(cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ../build/ocsf ./cmd/ocsf)

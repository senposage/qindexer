#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
VERSION="${VERSION:-1.0.2}"
COMMIT="local"

mkdir -p "$DIST"
if command -v git >/dev/null 2>&1; then
  COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo local)"
fi

LDFLAGS="-s -w -X qindexer/internal/version.Version=$VERSION -X qindexer/internal/version.Commit=$COMMIT"

cd "$ROOT"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/qindexer_windows_amd64.exe" ./cmd/qindexer
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/qindexer_linux_amd64" ./cmd/qindexer

ls -lh "$DIST"

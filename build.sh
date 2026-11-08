#!/usr/bin/env bash
# Builds every platform the way the published files are built. The same source and
# the Go version in go.mod give the same bytes; compare with SHA256SUMS.
set -euo pipefail
version="${1:?usage: build.sh VERSION [OUTDIR]}"
out="${2:-dist}"
mkdir -p "$out"
build() { # goos goarch name extra-ldflags
    CGO_ENABLED=0 GOOS="$1" GOARCH="$2" GOFLAGS='' go build -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid= -X main.version=$version $4" -o "$out/$3" ./cmd/relaycheck-client
}
build windows amd64 pinai-check-windows-amd64.exe '-H=windowsgui'
build darwin arm64 pinai-check-macos-arm64 ''
build darwin amd64 pinai-check-macos-amd64 ''
build linux amd64 pinai-check-linux-amd64 ''
(cd "$out" && sha256sum -b pinai-check-* > SHA256SUMS && cat SHA256SUMS)

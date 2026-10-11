#!/usr/bin/env bash
# Builds the browser explorer into dist/, ready for go-fileshare/portal:
#
#   ui "explorer" { dir = "/usr/share/fileshare-explorer" }   # dist/, copied there
#
# dist/ holds explorer.wasm and explorer.wasm.gz (the portal sends the .gz to a
# browser that accepts gzip), wasm_exec.js from the Go that built it -- the
# two must match -- and the page.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dist="$here/dist"
rm -rf "$dist"
mkdir -p "$dist"
( cd "$here" && GOOS=js GOARCH=wasm CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o "$dist/explorer.wasm" . )
gzip -9 -k "$dist/explorer.wasm"
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$dist/"
cp "$here"/web/* "$dist/"
ls -l "$dist"

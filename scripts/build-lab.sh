#!/usr/bin/env bash
# Builds the WebAssembly lab into web/lab/, ready to serve as static files.
#
# wasm_exec.js is copied from the local Go toolchain rather than committed,
# because it is part of the runtime and has to match the compiler that built
# the blob beside it. A vendored copy drifts silently on a Go upgrade and
# presents as a page that loads and then does nothing.
set -euo pipefail

cd "$(dirname "$0")/.."
out=web/lab

CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$out/lab.wasm" ./cmd/lab

goroot=$(go env GOROOT)
exec_js="$goroot/lib/wasm/wasm_exec.js"
if [ ! -f "$exec_js" ]; then
  # Before Go 1.24 it lived under misc/wasm.
  exec_js="$goroot/misc/wasm/wasm_exec.js"
fi
cp "$exec_js" "$out/wasm_exec.js"

size=$(wc -c < "$out/lab.wasm")
printf 'built %s (%.1f MB uncompressed)\n' "$out/lab.wasm" "$(echo "$size" | awk '{print $1/1048576}')"
echo "serve it with:  cd $out && python -m http.server 8080"
echo "note: gzip or brotli at the edge, the blob compresses to about a quarter of that."

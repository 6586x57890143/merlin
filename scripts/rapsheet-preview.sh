#!/usr/bin/env bash
# Render every user-facing rapsheet surface Discord-style in a browser.
#
# internal/plugins/rapsheet/preview_test.go drives the real handlers with the
# real voice catalogue and writes what they put on the wire as JSON;
# scripts/rapsheet-preview/index.html draws it the way Discord would (dark
# theme, embed colour bar, inline fields, buttons, mood thumbnail). Nothing
# here is a test of anything; it is how a change to the sheet, a DM or a
# mod-channel post gets looked at by a person before it ships.
#
#   scripts/rapsheet-preview.sh            # writes and serves on :8765
#   PORT=9000 scripts/rapsheet-preview.sh
#
# Output lands in .rapsheet-preview/ (gitignored). Ctrl-C stops the server.
set -euo pipefail
cd "$(dirname "$0")/.."
# python3 on most machines; on Windows the Store leaves a python3 stub that
# only prints an error, so the one that answers --version is the one used.
py=python3
if ! python3 --version >/dev/null 2>&1; then py=python; fi
out=".rapsheet-preview"
mkdir -p "$out/assets"
cp scripts/rapsheet-preview/index.html "$out/index.html"
cp internal/core/assets/merlin_*.png "$out/assets/"
RAPSHEET_PREVIEW_DIR="$PWD/$out" go test ./internal/plugins/rapsheet/ -run TestWritePreviews -count=1 >/dev/null
n=$($py -c "import json,sys;print(len(json.load(open('$out/scenes.json'))))" 2>/dev/null || echo "?")
echo "wrote $n scenes to $out/; serving http://localhost:${PORT:-8765}/"
cd "$out" && exec $py -m http.server "${PORT:-8765}"

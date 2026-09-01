#!/usr/bin/env bash
# Stages web/contest/public/ for `wrangler deploy`.
#
# All this does is copy merlin's mood drawings out of internal/core/assets,
# where they already live as the embed thumbnails, into the gallery's sticker
# folder. Same reasoning as build-lab.sh copying wasm_exec.js rather than
# committing it: the art has one owner, and a second copy in git is a second
# copy to forget about the day somebody redraws her.
#
# The stickers are decorative, so a deploy that skipped this step gets a page
# with six broken images and everything else working, which is the right way
# round for a build step to fail.
set -euo pipefail

cd "$(dirname "$0")/.."
out=web/contest/public/stickers
mkdir -p "$out"

# The six moods have one owner in internal/core/assets, where the bot embeds
# them as embed thumbnails. Copied rather than duplicated in git, so redrawing
# her updates both surfaces.
for mood in ok error warn info notice idle; do
  cp "internal/core/assets/merlin_$mood.png" "$out/merlin_$mood.png"
done

# Contest-only art, which nothing else uses and which therefore lives here.
cp web/contest/art/*.png "$out/"

echo "staged $(ls -1 "$out" | wc -l | tr -d ' ') stickers into $out"
echo "deploy with:  cd web/contest && wrangler deploy"

# drawably ships as native ESM with relative imports, so the gallery loads it
# straight from /vendor with no bundler, matching the no-build-step rule the
# page itself follows. Copied rather than committed for the same reason the
# mood art is: npm owns the version, package.json pins it, and a second copy
# in git is a second copy to forget the day it is bumped.
#
# A missing copy is survivable the same way missing stickers are: the page
# imports drawably dynamically and falls back to plain borders, so a skipped
# build step costs the drawing and never the contest.
pen=web/contest/public/vendor/drawably
if [ -d node_modules/drawably/dist ]; then
  mkdir -p "$pen"
  # react.js is deliberately not copied; this page is vanilla.
  for f in index controls rough prng; do
    cp "node_modules/drawably/dist/$f.js" "$pen/$f.js"
  done
  cp node_modules/drawably/style.css "$pen/style.css"
  echo "staged drawably into $pen"
else
  echo "warning: node_modules/drawably missing, run 'npm i'. the gallery will render unsketched." >&2
fi

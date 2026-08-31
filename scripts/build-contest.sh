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

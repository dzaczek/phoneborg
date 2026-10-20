#!/usr/bin/env bash
# Render every diagram source in src/ to a 2x PNG next to it.
# Needs rsvg-convert (brew install librsvg / apt install librsvg2-bin).
set -euo pipefail
cd "$(dirname "$0")"
for svg in src/*.svg; do
  name=$(basename "$svg" .svg)
  rsvg-convert -z 2 -o "$name.png" "$svg"
  echo "rendered $name.png"
done

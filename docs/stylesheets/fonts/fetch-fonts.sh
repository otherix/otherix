#!/bin/sh
# Download the self-hosted IBM Plex Sans / Mono woff2 files used by otherix.css.
# Run once from anywhere: sh stylesheets/fonts/fetch-fonts.sh
set -e
cd "$(dirname "$0")"
base="https://cdn.jsdelivr.net/fontsource/fonts"

for w in 400 500 600 700; do
  curl -fsSL "$base/ibm-plex-sans@latest/latin-$w-normal.woff2" -o "ibm-plex-sans-$w.woff2"
  echo "  ✓ ibm-plex-sans-$w.woff2"
done
for w in 400 500 600; do
  curl -fsSL "$base/ibm-plex-mono@latest/latin-$w-normal.woff2" -o "ibm-plex-mono-$w.woff2"
  echo "  ✓ ibm-plex-mono-$w.woff2"
done

echo "Fonts downloaded into docs/stylesheets/fonts/."

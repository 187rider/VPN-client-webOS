#!/usr/bin/env bash
# Pin a known upstream release asset; no Xray download occurs on LG TVs.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ -s "$ROOT/dist/payload/bin/xray" ] && [ -s "$ROOT/dist/payload/bin/geoip.dat" ] && [ -s "$ROOT/dist/payload/bin/geosite.dat" ]; then
    exit 0
fi
VERSION='v26.10.21'
ASSET='Xray-linux-arm32-v7a.zip'
# Official release asset digest, not a mutable latest URL.
SHA256='89978508fa2ebb4a1e661f2c6ed51d241c4091b3cd17f4c65fdf20baae7dedac'
URL="https://github.com/XTLS/Xray-core/releases/download/$VERSION/$ASSET"
mkdir -p "$ROOT/dist/payload/bin"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl --fail --location --retry 3 --proto '=https' --tlsv1.2 "$URL" -o "$TMP/$ASSET"
printf '%s  %s\n' "$SHA256" "$TMP/$ASSET" | sha256sum -c -
unzip -p "$TMP/$ASSET" xray > "$ROOT/dist/payload/bin/xray"
test -s "$ROOT/dist/payload/bin/xray"
chmod 700 "$ROOT/dist/payload/bin/xray"
unzip -p "$TMP/$ASSET" geoip.dat > "$ROOT/dist/payload/bin/geoip.dat"
test -s "$ROOT/dist/payload/bin/geoip.dat"
chmod 644 "$ROOT/dist/payload/bin/geoip.dat"
unzip -p "$TMP/$ASSET" geosite.dat > "$ROOT/dist/payload/bin/geosite.dat"
test -s "$ROOT/dist/payload/bin/geosite.dat"
chmod 644 "$ROOT/dist/payload/bin/geosite.dat"
curl --fail --location --retry 3 --proto '=https' --tlsv1.2 \
  "https://raw.githubusercontent.com/XTLS/Xray-core/$VERSION/LICENSE" \
  -o "$ROOT/dist/payload/XRAY-LICENSE.txt"
test -s "$ROOT/dist/payload/XRAY-LICENSE.txt"
printf 'XRAY_BUNDLED_VERSION=%s\n' "$VERSION"

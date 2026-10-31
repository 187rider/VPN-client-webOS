#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
D="$ROOT/dist/payload"
mkdir -p "$D/bin"
[[ -s "$ROOT/dist/cvpn-webos-panel-armv7" ]] || { echo 'ARMv7 panel not built' >&2; exit 1; }
bash "$ROOT/scripts/fetch-xray.sh"
cp "$ROOT/dist/cvpn-webos-panel-armv7" "$D/bin/panel"
cp "$ROOT/engine/cvpn.sh" "$D/bin/engine"
cp "$ROOT/recovery/recovery-worker.sh" "$D/bin/worker"
cp "$ROOT/recovery/boot-hook.sh" "$D/bin/boot-hook"
cp "$ROOT/scripts/start-panel.sh" "$D/bin/start-panel"
cp "$ROOT/scripts/stop-panel.sh" "$D/bin/stop-panel"
cp "$ROOT/scripts/install-bundled.sh" "$D/install.sh"
cp "$ROOT/server/config-template.json" "$D/client-template.json"
chmod 755 "$D/bin/boot-hook" "$D/install.sh"
(cd "$D" && sha256sum bin/xray bin/panel bin/engine bin/worker bin/boot-hook bin/start-panel bin/stop-panel bin/geoip.dat bin/geosite.dat > SHA256SUMS)

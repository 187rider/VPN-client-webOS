#!/bin/sh
# CVPN webOS 2.1.0 first-launch root installer, invoked by the Homebrew root service.
# No network downloads, no writes to system partitions, no automatic VPN activation.
set -eu
umask 077
ACTION=${1:-status}
APP=/media/developer/apps/usr/palm/applications/su.cvpn.webos
PAYLOAD="$APP/payload"
BASE=/media/developer/cvpn-webos
WEB="$BASE/webui"
HOOK=/var/lib/webosbrew/init.d/90-cvpn-webos
STAMP="$WEB/state/bundle.sha256"
FILES='bin/xray bin/panel bin/engine bin/worker bin/boot-hook bin/start-panel bin/stop-panel bin/geoip.dat bin/geosite.dat'
refuse() { echo "REFUSED=$1" >&2; exit 1; }
root_check() {
    [ "$(id -u)" = 0 ] || refuse root_required
    [ -f /var/lib/webosbrew/startup.sh ] || refuse homebrew_required
    arch=$(uname -m) || refuse arch_unknown
    case "$arch" in
        armv7l) : ;;
        aarch64) [ "$(getconf LONG_BIT 2>/dev/null)" = 32 ] || refuse armv7_userspace_required ;;
        *) refuse unsupported_arch ;;
    esac
    for cmd in sh ip awk od tr sha256sum readlink curl cp mv mkdir chmod ln date grep; do
        command -v "$cmd" >/dev/null 2>&1 || refuse "missing_tool:$cmd"
    done
    [ -c /dev/net/tun ] || refuse tun_unavailable
    [ -s "$PAYLOAD/SHA256SUMS" ] || refuse payload_missing
}
verify_payload() {
    (cd "$PAYLOAD" && sha256sum -c SHA256SUMS >/dev/null) || refuse payload_checksum_invalid
    for f in $FILES; do [ -s "$PAYLOAD/$f" ] || refuse "payload_missing:$f"; done
    for f in engine worker boot-hook start-panel stop-panel; do
        sh -n "$PAYLOAD/bin/$f" || refuse "invalid_shell:$f"
    done
    "$PAYLOAD/bin/xray" version >/dev/null 2>&1 || refuse incompatible_xray
}
manifest_hash() { sha256sum "$PAYLOAD/SHA256SUMS" | awk '{print $1}'; }
# Status is on the TV launcher's critical path; do not spend 4x5s
# waiting for backend health on EVERY relaunch. Longer retries are reserved
# for the first install/start and never delay an already-matching bundle.
panel_health() {
    [ -s "$WEB/access.key" ] || return 1
    key=$(cat "$WEB/access.key")
    count=0
    limit=4
    [ "${1:-}" = quick ] && limit=1
    while [ "$count" -lt "$limit" ]; do
        response=$(curl -fsS --noproxy '*' --connect-timeout 1 --max-time 3 \
            -H "X-CVPN-Key: $key" http://127.0.0.1:8765/api/status 2>/dev/null || true)
        case "$response" in *'"version":"2.0.0"'*) return 0;; esac
        count=$((count+1))
        [ "$count" -lt "$limit" ] && sleep 1
    done
    return 1
}
bundle_present() {
    [ -s "$STAMP" ] || return 1
    [ "$(cat "$STAMP")" = "$(manifest_hash)" ] || return 1
    [ -x "$WEB/bin/cvpn-webos-panel" ] || return 1
    [ -x "$BASE/bin/xray" ] || return 1
    [ -L "$HOOK" ] && [ "$(readlink "$HOOK")" = "$PAYLOAD/bin/boot-hook" ] || return 1
}
ready() {
    bundle_present || return 1
    panel_health "${1:-}" || return 1
}
# Detect a lock left by a killed installer/Engine without deleting locks
# owned by a live process or those with ambiguous/missing PID metadata.
# State is read-only until idle() has checked the VPN's routes and DNS mount.
lock_state() {
    [ -d "$BASE/state/lock" ] || { echo absent; return; }
    [ -s "$BASE/state/lock/pid" ] || { echo unknown; return; }
    lock_pid=$(cat "$BASE/state/lock/pid" 2>/dev/null || true)
    case "$lock_pid" in ''|*[!0-9]*) echo unknown; return;; esac
    if kill -0 "$lock_pid" 2>/dev/null; then
        echo busy
    else
        echo stale
    fi
}
idle() {
    [ ! -e "$BASE/state/active" ] || refuse vpn_running_disconnect_first
    if command -v pidof >/dev/null 2>&1 && [ -n "$(pidof xray 2>/dev/null || true)" ]; then
        refuse vpn_xray_present
    fi
    if ip link show dev cvpn0 >/dev/null 2>&1; then refuse vpn_tun_present; fi
    for block in 0.0.0.0/1 128.0.0.0/1; do
        route=$(ip -4 route show "$block") || refuse routes_unreadable
        case "$route" in *'dev cvpn0'*) refuse vpn_routes_present;; esac
    done
    if [ -r /proc/mounts ] && awk '$2=="/var/lib/misc/resolv.conf" {found=1} END {exit(!found)}' /proc/mounts; then
        refuse resolver_bind_present
    fi
    # Safe stale-lock recovery is limited to the local CVPN lock and only
    # occurs when no VPN state/resources exist. No system routes are changed.
    case "$(lock_state)" in
        absent) : ;;
        stale)
            old_pid=$(cat "$BASE/state/lock/pid") || refuse engine_busy
            case "$old_pid" in ''|*[!0-9]*) refuse engine_busy;; esac
            kill -0 "$old_pid" 2>/dev/null && refuse engine_busy
            rm -f "$BASE/state/lock/pid" || refuse engine_busy
            rmdir "$BASE/state/lock" 2>/dev/null || refuse engine_busy
            ;;
        *) refuse engine_busy ;;
    esac
}
# On any partial update, restore CVPN binaries but never touch user secrets.
restore_on_exit() {
    code=$?
    trap - 0
    if [ "$code" -ne 0 ] && [ "${RESTORE_ON_EXIT:-0}" = 1 ]; then
        if [ -x "$WEB/stop-panel.sh" ]; then sh "$WEB/stop-panel.sh" >/dev/null 2>&1 || true; fi
        for item in xray panel engine worker boot-hook start-panel stop-panel geoip geosite; do
            case "$item" in
                xray) dst="$BASE/bin/xray";; panel) dst="$WEB/bin/cvpn-webos-panel";;
                engine) dst="$BASE/bin/cvpn.sh";; worker) dst="$WEB/recovery/recovery-worker.sh";;
                boot-hook) dst="$WEB/recovery/boot-hook.sh";;
                start-panel|stop-panel) dst="$WEB/$item.sh";;
                geoip) dst="$BASE/bin/geoip.dat";; geosite) dst="$BASE/bin/geosite.dat";;
            esac
            if [ -s "$backup/$item" ]; then cp -p "$backup/$item" "$dst" || true;
            else rm -f "$dst" || true; fi
            rm -f "$dst".next.* 2>/dev/null || true
        done
        if [ -s "$backup/old-hook" ]; then
            ln -sfn "$(cat "$backup/old-hook")" "$HOOK" || true
        else
            rm -f "$HOOK" || true
        fi
        if [ -x "$WEB/start-panel.sh" ]; then sh "$WEB/start-panel.sh" >/dev/null 2>&1 || true; fi
        echo ROLLBACK_ATTEMPTED=YES >&2
    fi
    rm -f "$BASE/state/lock/pid" 2>/dev/null || true
    rmdir "$BASE/state/lock" 2>/dev/null || true
    exit "$code"
}
# Keep at most the two newest app-owned binary backups after a successful
# update. This limits flash usage on LG TVs with small /media/developer space.
# Never inspect or delete user profiles/configuration, and never prune on
# rollback/failure: those archives may be needed to restore the old version.
prune_binary_backups() {
    keep_prev=''
    keep_latest=''
    for entry in "$WEB"/state/backup-allinone-*; do
        [ -d "$entry" ] || continue
        if [ -n "$keep_prev" ]; then
            case "$keep_prev" in
                "$WEB"/state/backup-allinone-*) rm -rf "$keep_prev" 2>/dev/null || true ;;
            esac
        fi
        keep_prev=$keep_latest
        keep_latest=$entry
    done
}
install() {
    root_check
    if ready; then echo BUNDLE_READY=YES; exit 0; fi
    idle
    verify_payload
    [ ! -e "$HOOK" ] || [ -L "$HOOK" ] || refuse foreign_boot_hook
    if [ -L "$HOOK" ]; then
        case "$(readlink "$HOOK")" in
            "$WEB/recovery/boot-hook.sh"|"$PAYLOAD/bin/boot-hook") : ;;
            *) refuse foreign_boot_hook ;;
        esac
    fi
    mkdir -p "$BASE/state"
    chmod 700 "$BASE/state"
    # Share the Engine lock to avoid changing binaries during a concurrent VPN start.
    mkdir "$BASE/state/lock" 2>/dev/null || refuse engine_busy
    printf '%s\n' "$$" > "$BASE/state/lock/pid"
    trap 'restore_on_exit' 0
    [ ! -e "$BASE/state/active" ] || refuse vpn_running_disconnect_first
    if ip link show dev cvpn0 >/dev/null 2>&1; then refuse vpn_tun_present; fi
    mkdir -p "$BASE/bin" "$BASE/config" "$WEB/bin" "$WEB/secrets" "$WEB/state" "$WEB/recovery" /var/lib/webosbrew/init.d
    chmod 700 "$BASE/config" "$WEB/secrets" "$WEB/state"
    backup="$WEB/state/backup-allinone-$(date -u +%Y%m%d%H%M%S)-$$"
    mkdir -m 700 "$backup"
    [ ! -L "$HOOK" ] || readlink "$HOOK" > "$backup/old-hook"
    # Only executables and geo data are backed up; user credentials/profiles are untouched.
    for item in xray panel engine worker boot-hook start-panel stop-panel geoip geosite; do
        case "$item" in
            xray) dst="$BASE/bin/xray";;
            panel) dst="$WEB/bin/cvpn-webos-panel";;
            engine) dst="$BASE/bin/cvpn.sh";;
            worker) dst="$WEB/recovery/recovery-worker.sh";;
            boot-hook) dst="$WEB/recovery/boot-hook.sh";;
            start-panel|stop-panel) dst="$WEB/$item.sh";;
            geoip) dst="$BASE/bin/geoip.dat";;
            geosite) dst="$BASE/bin/geosite.dat";;
        esac
        [ ! -f "$dst" ] || cp -p "$dst" "$backup/$item"
    done
    RESTORE_ON_EXIT=1
    if [ -x "$WEB/stop-panel.sh" ]; then sh "$WEB/stop-panel.sh" || refuse panel_stop_failed; fi
    for item in xray panel engine worker boot-hook start-panel stop-panel geoip geosite; do
        case "$item" in
            xray) src="$PAYLOAD/bin/xray"; dst="$BASE/bin/xray";;
            panel) src="$PAYLOAD/bin/panel"; dst="$WEB/bin/cvpn-webos-panel";;
            engine) src="$PAYLOAD/bin/engine"; dst="$BASE/bin/cvpn.sh";;
            worker) src="$PAYLOAD/bin/worker"; dst="$WEB/recovery/recovery-worker.sh";;
            boot-hook) src="$PAYLOAD/bin/boot-hook"; dst="$WEB/recovery/boot-hook.sh";;
            start-panel|stop-panel) src="$PAYLOAD/bin/$item"; dst="$WEB/$item.sh";;
            geoip) src="$PAYLOAD/bin/geoip.dat"; dst="$BASE/bin/geoip.dat";;
            geosite) src="$PAYLOAD/bin/geosite.dat"; dst="$BASE/bin/geosite.dat";;
        esac
        cp "$src" "$dst.next.$$" || refuse payload_copy_failed
        chmod 700 "$dst.next.$$" || refuse payload_permissions_failed
        mv -f "$dst.next.$$" "$dst" || refuse payload_commit_failed
    done
    if [ ! -s "$BASE/config/client.json" ] && [ -s "$PAYLOAD/client-template.json" ]; then
        cp "$PAYLOAD/client-template.json" "$BASE/config/client.json"
        chmod 600 "$BASE/config/client.json"
    fi
    if [ ! -s "$WEB/access.key" ]; then
        [ ! -e "$WEB/access.key" ] || refuse existing_access_key_corrupt
        key=$(od -An -v -tx1 -N32 /dev/urandom | tr -d ' \n')
        [ "${#key}" = 64 ] || refuse key_generation_failed
        printf '%s\n' "$key" > "$WEB/access.key"
        chmod 600 "$WEB/access.key"
    fi
    # App-owned symlink becomes dangling after uninstall rather than autostarting old service.
    ln -sfn "$PAYLOAD/bin/boot-hook" "$HOOK" || refuse boot_hook_failed
    if ! sh "$WEB/start-panel.sh" || ! panel_health; then
        refuse panel_start_failed_original_restored
    fi
    printf '%s\n' "$(manifest_hash)" > "$STAMP.next"
    chmod 600 "$STAMP.next"
    mv -f "$STAMP.next" "$STAMP"
    RESTORE_ON_EXIT=0
    prune_binary_backups
    echo BUNDLE_INSTALLED=YES
    echo BUNDLE_READY=YES
    echo VPN_AUTOSTART=NO
    echo USER_DATA_PRESERVED=YES
}
case "$ACTION" in
    status)
        root_check
        if ready quick; then
            echo BUNDLE_READY=YES
        else
            echo BUNDLE_READY=NO
            if [ -e "$BASE/state/active" ]; then echo UPGRADE_BLOCKED=VPN_RUNNING; fi
            # A timed-out launcher must only re-check the in-flight install;
            # do not start another installer while it owns the Engine lock.
            case "$(lock_state)" in
                stale) echo STALE_LOCK_RECOVERABLE=YES ;;
                busy|unknown) echo INSTALL_IN_PROGRESS=YES ;;
            esac
        fi
        ;;
    install) install ;;
    *) refuse invalid_action ;;
esac

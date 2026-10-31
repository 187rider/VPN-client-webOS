#!/bin/sh
# CVPN webOS Engine 0.1.4 - LG webOS 5 / BusyBox ash; root-only experimental VPN.
# Full IPv4 TUN via Xray Reality/XHTTP, protected DNS via temporary resolv.conf bind mount.
set -u
umask 077

BASE=/media/developer/cvpn-webos
STATE="$BASE/state"
XRAY="$BASE/bin/xray"
CONFIG="$BASE/config/client.json"
RUN_CONFIG="$STATE/client.runtime.json"
SERVER_FILE="$BASE/config/server-ip.txt"
SELF="$BASE/bin/cvpn.sh"
IFACE=cvpn0
DNS=8.8.8.8
DNS_TARGET=/var/lib/misc/resolv.conf
RESOLV=/etc/resolv.conf
TRIAL_SECONDS=90
TEST_URL=https://www.youtube.com/favicon.ico

say() { echo "[CVPN webOS] $*"; }
fail() { say "ERROR: $*" >&2; exit 1; }
require_root() { [ "$(id -u)" = 0 ] || fail 'Root required'; }

# Discover the actual egress interface to the VPN endpoint, not a hard-coded
# eth0. Fail closed on missing/ambiguous routes or interface names.
detect_wan() {
    endpoint=$1
    route=$(ip -4 route get "$endpoint" 2>/dev/null) || return 1
    # The route to the remote server must go via a normal gateway.
    # A direct/point-to-point route needs an explicit reviewed implementation.
    gw=$(printf '%s\n' "$route" | awk 'NR==1 {for(i=1;i<=NF;i++) if($i=="via") {print $(i+1); exit}}')
    wan=$(printf '%s\n' "$route" | awk 'NR==1 {for(i=1;i<=NF;i++) if($i=="dev") {print $(i+1); exit}}')
    # Only interfaces also permitted by the panel's LAN subnet policy.
    case "$wan" in
        eth0|wlan0|wlan1) : ;;
        *) return 1 ;;
    esac
    case "$gw" in
        ''|*[!0-9.]*) return 1 ;;
    esac
    # Verify that the gateway belongs to an installed default route.
    if ! ip -4 route show default | awk -v w="$wan" -v g="$gw" '
        $1=="default" { for (i=1;i<=NF;i++) {
            if ($i=="via" && $(i+1)==g) foundgw=1
            if ($i=="dev" && $(i+1)==w) founddev=1
        }
        if (foundgw && founddev) ok=1
        foundgw=0; founddev=0
        } END{exit(ok?0:1)}'; then
        return 1
    fi
    WAN=$wan
    GATEWAY=$gw
}

# Derive an ephemeral runtime Xray config; the user's original client.json is
# never rewritten simply because the TV changed Ethernet/Wi-Fi connections.
prepare_runtime_config() {
    grep -q '"autoOutboundsInterface"' "$CONFIG" || return 1
    sed 's/"autoOutboundsInterface"[[:space:]]*:[[:space:]]*"[^"]*"/"autoOutboundsInterface":"'"$WAN"'"/g' "$CONFIG" > "$RUN_CONFIG.tmp.$$" || return 1
    chmod 600 "$RUN_CONFIG.tmp.$$" || return 1
    grep -q '"autoOutboundsInterface":"'"$WAN"'"' "$RUN_CONFIG.tmp.$$" || return 1
    mv -f "$RUN_CONFIG.tmp.$$" "$RUN_CONFIG" || return 1
}

mounted_resolver() {
    awk -v dest="$DNS_TARGET" '$2 == dest { found=1 } END { exit(found ? 0 : 1) }' /proc/mounts
}

lock() {
    tries=0
    while ! mkdir "$STATE/lock" 2>/dev/null; do
        tries=$((tries + 1))
        [ "$tries" -lt 12 ] || fail "Lock busy: $STATE/lock"
        if [ -f "$STATE/lock/pid" ]; then
            holder=$(cat "$STATE/lock/pid" 2>/dev/null || echo 0)
            case "$holder" in
                ''|*[!0-9]*) ;;
                *)
                    if ! kill -0 "$holder" 2>/dev/null; then
                        rm -f "$STATE/lock/pid"
                        rmdir "$STATE/lock" 2>/dev/null || true
                    fi
                    ;;
            esac
        fi
        sleep 1
    done
    echo $$ > "$STATE/lock/pid"
}
unlock() { rm -f "$STATE/lock/pid"; rmdir "$STATE/lock" 2>/dev/null || true; }
read_token() { [ -f "$STATE/active" ] && cat "$STATE/active" || true; }

owned_xray() {
    [ -s "$STATE/xray.pid" ] || return 1
    xp=$(cat "$STATE/xray.pid")
    case "$xp" in ''|*[!0-9]*) return 1 ;; esac
    [ -d "/proc/$xp" ] || return 1
    exe=$(readlink "/proc/$xp/exe" 2>/dev/null || true)
    [ "$exe" = "$XRAY" ] || return 1
    cmd=$(tr '\000' ' ' < "/proc/$xp/cmdline" 2>/dev/null || true)
    case "$cmd" in *"$RUN_CONFIG"*) return 0 ;; *) return 1 ;; esac
}

restore_route() {
    dst=$1
    snap=$2
    if [ -s "$snap" ]; then
        # Snapshot saved from kernel ip route show, not arbitrary user input.
        ip -4 route replace $(cat "$snap") || return 1
    else
        ip -4 route del "$dst/32" 2>/dev/null || true
    fi
}

rollback_unlocked() {
    expected=${1:-}
    current=$(read_token)
    [ -n "$current" ] || { say 'Already stopped'; return 0; }
    if [ -n "$expected" ] && [ "$current" != "$expected" ]; then
        say 'Ignoring recovery for an earlier session'
        return 0
    fi
    say 'Restoring network and DNS...'
    rc=0
    ip -4 route del 0.0.0.0/1 dev "$IFACE" 2>/dev/null || true
    ip -4 route del 128.0.0.0/1 dev "$IFACE" 2>/dev/null || true
    restore_route "$DNS" "$STATE/dns.route" || rc=1
    # Never use the mutable server-ip.txt for rollback of a running session.
    # Compatibility fallback is only for sessions started with Engine 0.1.1.
    if [ -s "$STATE/active-server-ip" ]; then
        server=$(cat "$STATE/active-server-ip" 2>/dev/null || true)
    else
        say 'WARNING: missing per-session server IP (old session); using current config'
        server=$(cat "$SERVER_FILE" 2>/dev/null || true)
    fi
    if [ -n "$server" ]; then
        restore_route "$server" "$STATE/server.route" || rc=1
    else
        say 'WARNING: cannot identify old server route for restoration'
        rc=1
    fi

    # Only our active session can unmount this path; preflight refuses pre-existing mounts.
    if mounted_resolver; then
        if umount "$DNS_TARGET"; then
            say 'DNS_MOUNT_RESTORED=YES'
        else
            say 'DNS_MOUNT_RESTORED=FAILED'
            rc=1
        fi
    else
        say 'DNS_MOUNT_RESTORED=NOT_MOUNTED'
    fi

    if owned_xray; then
        kill "$(cat "$STATE/xray.pid")" 2>/dev/null || true
        sleep 1
        owned_xray && rc=1
    fi
    ip -4 route show 0.0.0.0/1 | grep -q "dev $IFACE" && rc=1
    ip -4 route show 128.0.0.0/1 | grep -q "dev $IFACE" && rc=1
    mounted_resolver && rc=1
    if [ "$rc" -eq 0 ]; then
        rm -f "$STATE/active" "$STATE/phase" "$STATE/mode" "$STATE/deadline" "$STATE/xray.pid" "$STATE/active-server-ip" "$STATE/dns.route" "$STATE/server.route" "$STATE/probe.general" "$STATE/probe.youtube" "$STATE/probe.dns" "$STATE/wan" "$STATE/gateway" "$RUN_CONFIG"
        say 'NETWORK_RESTORED=YES'
    else
        say 'NETWORK_RESTORED=CHECK_REQUIRED'
    fi
    ip -4 route show default
    return "$rc"
}

stop_now() {
    require_root
    mkdir -p "$STATE" || fail 'State directory unavailable'
    lock
    rollback_unlocked "${1:-}"
    rc=$?
    unlock
    return "$rc"
}

# Readiness probes use curl's system DNS resolver directly. Legacy webOS
# BusyBox nslookup can return an A record yet hang waiting on another DNS
# response and exit 143 under timeout; that must not reject a healthy tunnel.
# This check keeps TLS certificate validation, never uses --resolve or a
# proxy, and requires a real hostname lookup through the installed VPN routes.
# The start path has already installed split IPv4 routes and a bind-mounted
# resolver pointing at 8.8.8.8. The watchdog independently checks these.
probe() {
    rm -f "$STATE/probe.general" "$STATE/probe.youtube" "$STATE/probe.dns"
    say 'PROBE_1_VERIFIED_HTTPS_WITH_SYSTEM_DNS'
    attempt=1
    general_ok=0
    while [ "$attempt" -le 3 ]; do
        if curl -4 -fsS --noproxy '*' --connect-timeout 8 --max-time 15 \
            -o /dev/null 'https://example.com/' \
            > "$STATE/startup-fallback.out" 2> "$STATE/startup-fallback.err"; then
            general_ok=1
            break
        fi
        attempt=$((attempt + 1))
        [ "$attempt" -le 3 ] && sleep 1
    done
    if [ "$general_ok" -ne 1 ]; then
        say 'GENERAL_HTTPS=FAILED (DNS, VPN transport or verified TLS)'
        echo FAILED > "$STATE/probe.general"
        echo FAILED > "$STATE/probe.dns"
        return 1
    fi
    # Successful HTTPS by hostname confirms the resolver and TCP/TLS data
    # path are usable on the active VPN. It does not separately prove DNS
    # encryption or availability of any particular service such as YouTube.
    say 'GENERAL_HTTPS=OK (TLS verified; hostname resolved)'
    echo OK > "$STATE/probe.general"
    echo OK > "$STATE/probe.dns"
    say 'SYSTEM_DNS=OK (verified HTTPS hostname lookup, not an nslookup test)'

    say 'PROBE_2_YOUTUBE_HTTPS'
    attempt=1
    youtube_ok=0
    while [ "$attempt" -le 3 ]; do
        if curl -4 -fsS -I --noproxy '*' --connect-timeout 8 --max-time 12 \
            "$TEST_URL" > "$STATE/youtube.out" 2> "$STATE/youtube.err"; then
            youtube_ok=1
            break
        fi
        attempt=$((attempt + 1))
        [ "$attempt" -le 3 ] && sleep 1
    done
    if [ "$youtube_ok" -eq 1 ]; then
        say 'YOUTUBE_HTTPS=OK'
        echo OK > "$STATE/probe.youtube"
    else
        say 'YOUTUBE_HTTPS=UNAVAILABLE (does not invalidate general HTTPS)'
        echo UNAVAILABLE > "$STATE/probe.youtube"
    fi
    say 'STARTUP_READINESS=GENERAL_INTERNET_VERIFIED'
    return 0
}

# Watchdog does not begin health checks until initial startup/probes complete.
health_probe() {
    # A single remote site may be down/blocked while the tunnel still works.
    # Requiring failures on two independent HTTPS sites avoids needless VPN shutdowns.
    if curl -4 -fsS -I --noproxy '*' --connect-timeout 7 --max-time 18 \
        -o /dev/null "$TEST_URL" > "$STATE/health.out" 2> "$STATE/health.err"; then
        return 0
    fi
    curl -4 -fsS --noproxy '*' --connect-timeout 7 --max-time 18 \
        -o /dev/null 'https://example.com/' > "$STATE/health-fallback.out" 2> "$STATE/health-fallback.err"
}

start_now() {
    require_root
    [ -x "$XRAY" ] || fail "Missing Xray: $XRAY"
    [ -f "$CONFIG" ] || fail "Missing config: $CONFIG"
    [ -s "$SERVER_FILE" ] || fail 'Missing server IP'
    for tool in nohup curl mount umount awk; do
        command -v "$tool" >/dev/null 2>&1 || fail "Missing tool: $tool"
    done
    mkdir -p "$STATE" || fail 'Cannot create state'
    chmod 700 "$STATE"
    lock
    if [ -f "$STATE/active" ]; then unlock; fail 'VPN session exists; use status/stop'; fi
    if ip link show dev "$IFACE" >/dev/null 2>&1; then unlock; fail "TUN $IFACE already exists"; fi
    if [ -n "$(ip -4 route show 0.0.0.0/1)" ] || [ -n "$(ip -4 route show 128.0.0.0/1)" ]; then
        unlock; fail 'Split routes already exist'
    fi
    if [ "$(readlink "$RESOLV" 2>/dev/null)" != "$DNS_TARGET" ] || [ ! -f "$DNS_TARGET" ]; then
        unlock; fail 'Unexpected /etc/resolv.conf target; refusing DNS override'
    fi
    if mounted_resolver; then unlock; fail 'Resolver already mounted; refusing to replace'; fi
    # Engine only captures IPv4; reject an unprotected IPv6 default route.
    if [ -n "$(ip -6 route show default 2>/dev/null)" ]; then
        unlock; fail 'IPv6 default route exists, but this VPN protects IPv4 only; disable IPv6 or add IPv6 tunneling'
    fi
    server=$(cat "$SERVER_FILE")
    case "$server" in ''|*[!0-9.]*) unlock; fail 'Expected literal IPv4 server';; esac
    if ! detect_wan "$server"; then
        unlock; fail 'No safe default route to VPN server (Ethernet/Wi-Fi); VPN unchanged'
    fi
    if ! prepare_runtime_config; then
        unlock; fail 'Unable to prepare runtime Xray config for current network'
    fi
    if ! "$XRAY" run -test -c "$RUN_CONFIG" > "$STATE/config-check.log" 2>&1; then
        rm -f "$RUN_CONFIG"
        unlock; fail 'Xray runtime config validation failed'
    fi

    # Staging does not alter ConnMan's underlying resolver file.
    printf 'nameserver %s\n' "$DNS" > "$STATE/resolv.vpn" || { unlock; fail 'Could not stage DNS'; }
    chmod 600 "$STATE/resolv.vpn"
    ip -4 route show "$DNS/32" > "$STATE/dns.route" || { unlock; fail 'Cannot snapshot DNS route'; }
    ip -4 route show "$server/32" > "$STATE/server.route" || { unlock; fail 'Cannot snapshot server route'; }
    # Snapshot endpoint address BEFORE any routes are changed. server-ip.txt can
    # change during subscription updates; rollback must use this original IP.
    printf '%s\n' "$server" > "$STATE/active-server-ip" || { unlock; fail 'Cannot save server IP'; }
    token="$(date +%s)-$$"
    echo "$token" > "$STATE/active"
    echo trial > "$STATE/mode"
    echo preparing > "$STATE/phase"
    echo "$(($(date +%s) + TRIAL_SECONDS))" > "$STATE/deadline"
    echo "$GATEWAY" > "$STATE/gateway"
    echo "$WAN" > "$STATE/wan"
    rm -f "$STATE/xray.pid" "$STATE/probe.general" "$STATE/probe.youtube" "$STATE/probe.dns"

    # Independent recovery processes are armed before changing routes/DNS.
    nohup sh "$SELF" guard "$token" > "$STATE/watchdog.log" 2>&1 < /dev/null &
    wp=$!
    nohup sh "$SELF" timer "$token" > "$STATE/timer.log" 2>&1 < /dev/null &
    tp=$!
    sleep 1
    if ! kill -0 "$wp" 2>/dev/null || ! kill -0 "$tp" 2>/dev/null; then
        rollback_unlocked "$token"; unlock; fail 'Safety processes failed to start'
    fi

    nohup "$XRAY" run -c "$RUN_CONFIG" > "$STATE/xray.log" 2>&1 < /dev/null &
    echo "$!" > "$STATE/xray.pid"
    # Slow webOS TVs may need longer than three seconds for the TUN device.
    # Check every second, bounded to 10s; never change routing before ready.
    wait_tun=0
    while [ "$wait_tun" -lt 10 ]; do
        if owned_xray && ip link show dev "$IFACE" >/dev/null 2>&1; then
            break
        fi
        if ! owned_xray; then
            rollback_unlocked "$token"; unlock; fail 'Xray exited before TUN became ready'
        fi
        wait_tun=$((wait_tun + 1))
        sleep 1
    done
    if ! owned_xray || ! ip link show dev "$IFACE" >/dev/null 2>&1; then
        rollback_unlocked "$token"; unlock; fail 'Xray/TUN not ready within 10 seconds'
    fi

    ok=1
    ip -4 route replace "$server/32" via "$GATEWAY" dev "$WAN" || ok=0
    if [ "$ok" -eq 1 ]; then ip -4 route add 0.0.0.0/1 dev "$IFACE" || ok=0; fi
    if [ "$ok" -eq 1 ]; then ip -4 route add 128.0.0.0/1 dev "$IFACE" || ok=0; fi
    if [ "$ok" -eq 1 ]; then ip -4 route replace "$DNS/32" dev "$IFACE" || ok=0; fi
    if [ "$ok" -eq 1 ]; then mount -o bind "$STATE/resolv.vpn" "$DNS_TARGET" || ok=0; fi
    if [ "$ok" -eq 1 ] && ! grep -qx "nameserver $DNS" "$RESOLV"; then ok=0; fi
    if [ "$ok" -eq 0 ]; then
        rollback_unlocked "$token"; unlock; fail 'DNS/routing setup failed and rollback was attempted'
    fi
    # Only after all routes and DNS are installed may the guard inspect them.
    echo probing > "$STATE/phase" || { rollback_unlocked "$token"; unlock; fail 'Cannot record probe phase'; }
    unlock

    say 'DNS override active without modifying ConnMan data'
    say 'Checking general HTTPS, DNS and YouTube reachability...'
    if ! probe; then
        stop_now "$token" || true
        fail 'Startup check failed; recovery attempted. Inspect logs.'
    fi
    # Allow guard to begin periodic probes only after startup checks complete.
    lock
    if [ "$(read_token)" != "$token" ]; then
        unlock
        fail 'Session ended while probing; check status'
    fi
    echo ready > "$STATE/phase" || { unlock; fail 'Cannot mark session ready'; }
    unlock
    say 'VPN_CONNECTED=YES'
    say "SESSION=$token"
    say "TRIAL_EXPIRES_AFTER=$TRIAL_SECONDS seconds from start"
    say "Use 'confirm' only after verifying TV operation."
}

confirm_now() {
    require_root
    mkdir -p "$STATE"
    lock
    if [ ! -f "$STATE/active" ] || ! owned_xray || [ "$(cat "$STATE/phase" 2>/dev/null)" != ready ] || ! mounted_resolver; then
        unlock; fail 'No healthy running session to confirm'
    fi
    # A late request must not rescue an already expired trial.
    if [ "$(cat "$STATE/mode" 2>/dev/null)" = trial ]; then
        deadline=$(cat "$STATE/deadline" 2>/dev/null || echo 0)
        case "$deadline" in ''|*[!0-9]*) deadline=0 ;; esac
        if [ "$(date +%s)" -ge "$deadline" ]; then
            unlock; fail 'Trial already expired; reconnect instead of confirming'
        fi
    fi
    echo persistent > "$STATE/mode" || { unlock; fail 'Cannot confirm session'; }
    rm -f "$STATE/deadline"
    unlock
    say 'VPN_CONFIRMED=YES (guard remains active)'
}

watch_now() {
    expected=$1
    fails=0
    sleep 8
    while :; do
        [ "$(read_token)" = "$expected" ] || exit 0
        mode=$(cat "$STATE/mode" 2>/dev/null || echo trial)
        if [ "$mode" = trial ]; then
            dl=$(cat "$STATE/deadline" 2>/dev/null || echo 0)
            if [ "$(date +%s)" -ge "$dl" ]; then
                say 'Trial expired; restoring'; stop_now "$expected"; exit $?
            fi
        fi
        phase=$(cat "$STATE/phase" 2>/dev/null || echo preparing)
        # The Engine owns the startup transition. Do not interpret missing TUN
        # or partial routes as failure before route installation has completed.
        # The independent trial timer still restores a stalled startup.
        if [ "$phase" = preparing ]; then
            sleep 2
            continue
        fi
        if ! owned_xray || ! ip link show dev "$IFACE" >/dev/null 2>&1; then
            say 'Xray/TUN lost; restoring'; stop_now "$expected"; exit $?
        fi
        if ! ip -4 route show 0.0.0.0/1 | grep -q "dev $IFACE" ||
           ! ip -4 route show 128.0.0.0/1 | grep -q "dev $IFACE" ||
           ! ip -4 route show "$DNS/32" | grep -q "dev $IFACE" ||
           ! mounted_resolver ||
           ! grep -qx "nameserver $DNS" "$RESOLV"; then
            say 'Routing or DNS mount changed; restoring'; stop_now "$expected"; exit $?
        fi
        if [ "$phase" = ready ]; then
            if health_probe; then
                fails=0
            else
                fails=$((fails + 1))
                say "HTTPS health failure ($fails/2): $(cat "$STATE/health.err" 2>/dev/null)"
                if [ "$fails" -ge 2 ]; then
                    say 'Health probe failed twice; restoring'; stop_now "$expected"; exit $?
                fi
            fi
        fi
        sleep 10
    done
}

timer_now() {
    expected=$1
    sleep $((TRIAL_SECONDS + 5))
    [ "$(read_token)" = "$expected" ] || exit 0
    mode=$(cat "$STATE/mode" 2>/dev/null || echo trial)
    if [ "$mode" = trial ]; then
        say 'Trial deadline reached; restoring'
        stop_now "$expected"
    fi
}

status_now() {
    say "Version: 0.1.4"
    say "Session: $(read_token)"
    [ -f "$STATE/mode" ] && say "Mode: $(cat "$STATE/mode")"
    [ -f "$STATE/phase" ] && say "Phase: $(cat "$STATE/phase")"
    if owned_xray; then say 'Xray: running'; else say 'Xray: stopped'; fi
    if mounted_resolver; then say 'DNS override: mounted'; else say 'DNS override: not mounted'; fi
    for component in general dns youtube; do
        f="$STATE/probe.$component"
        [ -s "$f" ] && say "Initial $component: $(cat "$f")"
    done
    [ -s "$STATE/wan" ] && say "WAN: $(cat "$STATE/wan")"
    say 'resolv.conf:'
    cat "$RESOLV" 2>/dev/null || true
    say 'Routes:'
    ip -4 route show 0.0.0.0/1
    ip -4 route show 128.0.0.0/1
    ip -4 route show "$DNS/32"
    ip -4 route show default
    say "Logs: $STATE/xray.log, $STATE/watchdog.log"
}

case "${1:-}" in
    start) start_now ;;
    stop) stop_now ;;
    confirm) confirm_now ;;
    status) status_now ;;
    guard) [ -n "${2:-}" ] || fail 'Missing token'; watch_now "$2" ;;
    timer) [ -n "${2:-}" ] || fail 'Missing token'; timer_now "$2" ;;
    *) echo 'CVPN webOS Engine 0.1.4: start | status | confirm | stop'; exit 2 ;;
esac

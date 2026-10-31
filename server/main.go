package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed public/*
var frontend embed.FS

//go:embed config-template.json
var embeddedClientTemplate []byte

const version = "2.0.0"

var base = "/media/developer/cvpn-webos"
var address = ":8765"
var mx sync.Mutex
var uuidRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var safeID = regexp.MustCompile(`^[0-9a-f]{24}$`)
var ipErr = errors.New("endpoint has no valid IPv4 address")

type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Transport   string `json:"transport"`
	Security    string `json:"security"`
	Source      string `json:"source"`
	Secret      string `json:"-"`
	GroupID     string `json:"groupId,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
type profilesFile struct {
	Active string    `json:"active"`
	Items  []Profile `json:"items"`
	Groups []Group   `json:"groups,omitempty"`
}
type app struct{ key string }

func path(parts ...string) string { return filepath.Join(append([]string{base}, parts...)...) }
func fail(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func readProfiles() (profilesFile, error) {
	var p profilesFile
	b, e := os.ReadFile(path("webui", "profiles.json"))
	if os.IsNotExist(e) {
		return p, nil
	}
	if e != nil {
		return p, e
	}
	e = json.Unmarshal(b, &p)
	return p, e
}
func atomic(pathname string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(pathname), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(pathname), ".cvpn-staging-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), pathname)
}
func writeProfiles(p profilesFile) error {
	return atomic(path("webui", "profiles.json"), mustJSON(p), 0600)
}
func mustJSON(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return b }
func readConfig() (map[string]any, error) {
	b, e := os.ReadFile(path("config", "client.json"))
	if errors.Is(e, os.ErrNotExist) {
		// A clean installation has no selected server yet. Use only the
		// audited embedded TUN skeleton, never a copied user's VPN secret.
		b, e = embeddedClientTemplate, nil
	}
	if e != nil {
		return nil, e
	}
	var m map[string]any
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, e
	}
	return m, nil
}
func outbound(m map[string]any) (map[string]any, error) {
	a, _ := m["outbounds"].([]any)
	for _, e := range a {
		o, ok := e.(map[string]any)
		if ok && o["protocol"] == "vless" {
			return o, nil
		}
	}
	return nil, errors.New("VLESS outbound missing")
}
func metaFromOutbound(o map[string]any) (string, string, string, error) {
	settings, _ := o["settings"].(map[string]any)
	v, _ := settings["vnext"].([]any)
	if len(v) < 1 {
		return "", "", "", errors.New("VLESS vnext missing")
	}
	srv, _ := v[0].(map[string]any)
	host, _ := srv["address"].(string)
	if host == "" {
		return "", "", "", errors.New("server host missing")
	}
	stream, _ := o["streamSettings"].(map[string]any)
	tr, _ := stream["network"].(string)
	if tr == "" {
		tr = "tcp"
	}
	sec, _ := stream["security"].(string)
	if sec == "" {
		sec = "none"
	}
	return host, strings.ToUpper(tr), strings.ToUpper(sec), nil
}
func readExisting() (Profile, error) {
	m, err := readConfig()
	if err != nil {
		return Profile{}, err
	}
	o, e := outbound(m)
	if e != nil {
		return Profile{}, e
	}
	host, tr, sec, e := metaFromOutbound(o)
	return Profile{ID: "installed", Name: "Установленный профиль", Address: host, Transport: tr, Security: sec, Source: "existing"}, e
}
func publicList(p profilesFile) map[string]any {
	list := make([]map[string]string, 0, len(p.Items))
	for _, v := range p.Items {
		list = append(list, map[string]string{"id": v.ID, "name": v.Name, "address": v.Address,
			"transport": v.Transport, "security": v.Security, "source": v.Source, "groupId": v.GroupID})
	}
	groups := make([]map[string]any, 0, len(p.Groups))
	for _, g := range p.Groups {
		count := 0
		for _, item := range p.Items {
			if item.GroupID == g.ID {
				count++
			}
		}
		isOfficial := officialGroupSource(g)
		var exposedMeta *SubscriptionMeta
		if isOfficial {
			exposedMeta = g.Meta
		}
		groups = append(groups, map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "domain": g.Domain,
			"count": count, "updatedAt": g.UpdatedAt, "canRefresh": g.Kind == "subscription", "meta": exposedMeta, "officialCVPN": isOfficial})
	}
	return map[string]any{"active": p.Active, "items": list, "groups": groups}
}
func active() bool {
	b, e := os.ReadFile(path("state", "active"))
	return e == nil && len(strings.TrimSpace(string(b))) > 0
}
func status() map[string]any {
	p, _ := readProfiles()
	out := map[string]any{"version": version, "engine": engineVersion(), "connected": false, "phase": "stopped", "mode": "", "profile": p.Active}
	if _, err := os.Stat(path("bin", "cvpn.sh")); err != nil {
		out["engine"] = "not installed"
	}
	if active() {
		// Advisory startup results, not a streaming/UDP guarantee. Missing keys
		// are expected for older Engine sessions and must not block status.
		for _, spec := range []struct{ key, file string }{
			{"generalHTTPS", "probe.general"}, {"startupDNS", "probe.dns"}, {"youtubeHTTPS", "probe.youtube"},
		} {
			if b, err := os.ReadFile(path("state", spec.file)); err == nil {
				switch v := strings.TrimSpace(string(b)); v {
				case "OK", "FAILED", "UNAVAILABLE":
					out[spec.key] = v
				}
			}
		}
		out["reachabilityNote"] = "Начальные HTTPS-проверки не подтверждают видеопоток, UDP/QUIC или доступность всех сайтов."
		out["connected"] = true
		for _, name := range []string{"phase", "mode"} {
			b, _ := os.ReadFile(path("state", name))
			out[name] = strings.TrimSpace(string(b))
		}
		if out["mode"] == "trial" {
			b, err := os.ReadFile(path("state", "deadline"))
			if err == nil {
				deadline, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
				if err == nil {
					remaining := deadline - time.Now().Unix()
					if remaining < 0 {
						remaining = 0
					}
					out["trialRemainingSec"] = remaining
				}
			}
		}
		phase, _ := out["phase"].(string)
		if phase == "ready" {
			probe := runtimeProbe()
			if !probe.Healthy {
				out["connected"] = false
				out["phase"] = "degraded"
				out["needsRecovery"] = true
				out["reason"] = probe.Reason
			} else if trialExpired() && out["mode"] == "trial" {
				out["phase"] = "expiring"
			}
		}
	}
	return out
}
func jsonResponse(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}
func errorOut(w http.ResponseWriter, code int, msg string) {
	jsonResponse(w, code, map[string]any{"ok": false, "error": msg})
}
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		errorOut(w, 405, "POST required")
		return false
	}
	return true
}
func clientAuthorized(r *http.Request, key string) bool {
	got := r.Header.Get("X-CVPN-Key")
	if len(got) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}
func localOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, e := url.Parse(o)
	if e != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra JSON content")
	}
	return nil
}
func (a *app) api(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Permitted-Cross-Domain-Policies", "none")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if !localOrigin(r) {
		errorOut(w, 403, "origin not allowed")
		return
	}
	if a.handlePairingAPI(w, r) {
		return
	}
	if exceededFailures(r.RemoteAddr) {
		errorOut(w, 429, "Слишком много попыток входа. Попробуйте позже.")
		return
	}
	legacy := localTVRequest(r) && clientAuthorized(r, a.key)
	if !legacy {
		sess, ok := readBrowserSession(r)
		if !ok {
			recordFailure(r.RemoteAddr)
			errorOut(w, 401, "session required")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !constantEqual(r.Header.Get("X-CVPN-CSRF"), sess.CSRF) {
			errorOut(w, 403, "request validation failed")
			return
		}
	}
	clearFailures(r.RemoteAddr)
	if a.handleDeviceAPI(w, r, legacy) {
		return
	}
	if r.URL.Path == "/api/language" {
		a.languageAPI(w, r)
		return
	}
	if r.URL.Path == "/api/release/health" && r.Method == "GET" {
		jsonResponse(w, 200, releaseHealth())
		return
	}
	if r.URL.Path == "/api/status" && r.Method == "GET" {
		jsonResponse(w, 200, status())
		return
	}
	if r.URL.Path == "/api/profiles" && r.Method == "GET" {
		p, e := readProfiles()
		if e != nil {
			errorOut(w, 500, "storage unavailable")
			return
		}
		jsonResponse(w, 200, publicList(p))
		return
	}
	if r.URL.Path == "/api/connect" {
		if !requirePost(w, r) {
			return
		}
		a.connectProfile(w, r)
		return
	}
	if r.URL.Path == "/api/groups/rename" || r.URL.Path == "/api/groups/delete" || r.URL.Path == "/api/groups/refresh" {
		if !requirePost(w, r) {
			return
		}
		a.groupAction(w, r)
		return
	}
	if r.URL.Path == "/api/groups/ping" {
		if !requirePost(w, r) {
			return
		}
		a.subscriptionPing(w, r)
		return
	}
	if r.URL.Path == "/api/diagnostics" && r.Method == "GET" {
		jsonResponse(w, 200, diagnostics())
		return
	}
	if r.URL.Path == "/api/recovery/check" && r.Method == "GET" {
		jsonResponse(w, 200, networkRecoverySnapshot())
		return
	}
	if r.URL.Path == "/api/recovery/restore" {
		if !requirePost(w, r) {
			return
		}
		a.restoreNetwork(w, r)
		return
	}
	if r.URL.Path == "/api/engine" {
		if !requirePost(w, r) {
			return
		}
		a.engine(w, r)
		return
	}
	if r.URL.Path == "/api/import" {
		if !requirePost(w, r) {
			return
		}
		a.importProfile(w, r)
		return
	}
	if r.URL.Path == "/api/select" {
		if !requirePost(w, r) {
			return
		}
		a.selectProfile(w, r)
		return
	}
	if r.URL.Path == "/api/logs" && r.Method == "GET" {
		jsonResponse(w, 200, map[string]any{"logs": safeEventSummary() + "\n\n" + safeEngineLogSummary()})
		return
	}
	if r.URL.Path == "/api/geoip/status" && r.Method == "GET" {
		a.handleGeoIPStatus(w, r)
		return
	}
	if r.URL.Path == "/api/geoip/settings" {
		if !requirePost(w, r) {
			return
		}
		a.handleGeoIPSettings(w, r)
		return
	}
	if r.URL.Path == "/api/geoip/update" {
		if !requirePost(w, r) {
			return
		}
		a.handleGeoIPManualUpdate(w, r)
		return
	}
	if r.URL.Path == "/api/autostart" && r.Method == "GET" {
		a.handleAutostartStatus(w, r)
		return
	}
	if r.URL.Path == "/api/autostart" {
		if !requirePost(w, r) {
			return
		}
		a.handleAutostartSave(w, r)
		return
	}
	errorOut(w, 404, "unknown endpoint")
}
func (a *app) engine(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action string `json:"action"`
	}
	if e := decodeJSON(r, &b); e != nil {
		errorOut(w, 400, "invalid request")
		return
	}
	switch b.Action {
	case "start", "stop", "confirm", "status":
	default:
		errorOut(w, 400, "unknown action")
		return
	}
	mx.Lock()
	defer mx.Unlock()
	if b.Action == "start" && (engineIsTransitioning() || active()) {
		errorOut(w, 409, "VPN уже запускается или выполняется другая операция")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()
	// Acknowledge the user action immediately instead of waiting 10–16 seconds
	// for the read-only observer and external public-IP lookup.
	if b.Action == "start" {
		p, _ := readProfiles()
		announceVPNConnecting(p.Active, false)
	} else if b.Action == "stop" {
		announceVPNDisconnecting()
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", path("bin", "cvpn.sh"), b.Action)
	out, err := cmd.CombinedOutput()
	msg := string(out)
	if len(msg) > 6000 {
		msg = msg[len(msg)-6000:]
	}
	if err != nil {
		log.Printf("engine action %s failed: %v", b.Action, err)
		recordEngineAction(b.Action, false)
		if b.Action == "start" || b.Action == "stop" {
			announceVPNActionError(b.Action)
		}
		errorOut(w, 409, redactSecrets(strings.TrimSpace(msg)))
		return
	}
	recordEngineAction(b.Action, true)
	if b.Action == "start" {
		s := status()
		if s["connected"] == true && s["phase"] == "ready" && s["needsRecovery"] != true {
			p, _ := readProfiles()
			announceVPNConnected(p.Active)
		} else {
			announceVPNActionError(b.Action)
		}
	} else if b.Action == "stop" {
		if directNetworkRestored() {
			announceVPNDisconnected()
		} else {
			announceVPNActionError(b.Action)
		}
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "message": msg, "status": status()})
}
func randomID() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func resolveIPv4(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil && publicDestination(v4) {
			return v4.String(), nil
		}
		return "", ipErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	for _, i := range ips {
		if v4 := i.To4(); v4 != nil && publicDestination(v4) {
			return v4.String(), nil
		}
	}
	return "", ipErr
}
func makeVLESS(s string) (map[string]any, string, string, error) {
	u, e := url.Parse(strings.TrimSpace(s))
	if e != nil || !strings.EqualFold(u.Scheme, "vless") || u.User == nil {
		return nil, "", "", errors.New("expected vless:// link")
	}
	uid := u.User.Username()
	if !uuidRE.MatchString(uid) {
		return nil, "", "", errors.New("invalid VLESS UUID")
	}
	host := u.Hostname()
	portStr := u.Port()
	port, e := strconv.Atoi(portStr)
	if e != nil || port < 1 || port > 65535 {
		return nil, "", "", errors.New("invalid port")
	}
	ip, e := resolveIPv4(host)
	if e != nil {
		return nil, "", "", errors.New("server IPv4 unresolved")
	}
	q := u.Query()
	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}
	if network == "gun" { // V2Ray's historical name for the gRPC transport.
		network = "grpc"
	}
	if network != "tcp" && network != "xhttp" && network != "grpc" {
		return nil, "", "", errors.New("supported transports: TCP, XHTTP, gRPC")
	}
	sec := strings.ToLower(q.Get("security"))
	if sec != "reality" && !(network == "grpc" && sec == "tls") {
		return nil, "", "", errors.New("supported security: Reality, gRPC also supports TLS")
	}
	pbk := q.Get("pbk")
	sni := strings.TrimSpace(q.Get("sni"))
	if sec == "reality" && (pbk == "" || sni == "") {
		return nil, "", "", errors.New("Reality pbk and sni required")
	}
	if sec == "tls" && sni == "" {
		sni = host // Preserve the original TLS domain before resolving IPv4.
	}
	fp := q.Get("fp")
	if fp == "" {
		fp = "chrome"
	}
	user := map[string]any{"id": uid, "encryption": "none"}
	if flow := q.Get("flow"); flow != "" {
		if network != "tcp" {
			return nil, "", "", errors.New("flow must be empty for gRPC and XHTTP")
		}
		user["flow"] = flow
	}
	stream := map[string]any{"network": network, "security": sec}
	if sec == "reality" {
		stream["realitySettings"] = map[string]any{"serverName": sni, "fingerprint": fp, "password": pbk, "shortId": q.Get("sid"), "spiderX": q.Get("spx")}
	} else {
		// gRPC uses HTTP/2. Never allow an imported link to disable TLS verification.
		if isTruthy(q.Get("allowInsecure")) || isTruthy(q.Get("insecure")) {
			return nil, "", "", errors.New("insecure TLS certificates are not allowed")
		}
		tlsSettings := map[string]any{"serverName": sni, "alpn": []string{"h2"}, "fingerprint": fp}
		stream["tlsSettings"] = tlsSettings
	}
	if network == "grpc" {
		settings, err := grpcFromLink(q)
		if err != nil {
			return nil, "", "", err
		}
		stream["grpcSettings"] = settings
	}
	if network == "xhttp" {
		xh := map[string]any{"path": "/", "mode": "auto"}
		if q.Get("path") != "" {
			xh["path"] = q.Get("path")
		}
		if q.Get("mode") != "" {
			xh["mode"] = q.Get("mode")
		}
		if q.Get("host") != "" {
			xh["host"] = q.Get("host")
		}
		if extra := q.Get("extra"); extra != "" {
			if len(extra) > 12000 || !json.Valid([]byte(extra)) {
				return nil, "", "", errors.New("invalid XHTTP extra JSON")
			}
			var raw any
			if err := json.Unmarshal([]byte(extra), &raw); err != nil {
				return nil, "", "", errors.New("invalid XHTTP extra JSON")
			}
			if _, ok := raw.(map[string]any); !ok {
				return nil, "", "", errors.New("XHTTP extra must be a JSON object")
			}
			xh["extra"] = raw
		}
		stream["xhttpSettings"] = xh
	}
	out := map[string]any{"tag": "cvpn-proxy", "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": ip, "port": port, "users": []any{user}}}}, "streamSettings": stream}
	name, _ := url.PathUnescape(u.Fragment)
	name = strings.TrimSpace(name)
	if name == "" {
		name = host
	}
	if len([]rune(name)) > 70 {
		name = string([]rune(name)[:70])
	}
	return out, ip, name, nil
}
func makeJSON(s string) (map[string]any, string, string, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, "", "", errors.New("invalid JSON")
	}
	var o map[string]any
	if m["protocol"] == "vless" {
		o = m
	} else {
		o, _ = outbound(m)
	}
	if o == nil {
		return nil, "", "", errors.New("VLESS outbound not found")
	}
	addr, tr, sec, e := metaFromOutbound(o)
	if e != nil {
		return nil, "", "", e
	}
	if tr != "TCP" && tr != "XHTTP" && tr != "GRPC" {
		return nil, "", "", errors.New("only TCP, XHTTP and gRPC supported")
	}
	if sec != "REALITY" && !(tr == "GRPC" && sec == "TLS") {
		return nil, "", "", errors.New("supported security: Reality; gRPC also supports TLS")
	}
	ip, e := resolveIPv4(addr)
	if e != nil {
		return nil, "", "", e
	}
	// Strip unsupported client-controlled fields, preserve settings and streamSettings including XHTTP extra.
	settings, ok := o["settings"].(map[string]any)
	if !ok {
		return nil, "", "", errors.New("settings missing")
	}
	stream, ok := o["streamSettings"].(map[string]any)
	if !ok {
		return nil, "", "", errors.New("streamSettings missing")
	}
	v, _ := settings["vnext"].([]any)
	if len(v) != 1 {
		return nil, "", "", errors.New("exactly one VLESS endpoint required")
	}
	srv, ok := v[0].(map[string]any)
	if !ok {
		return nil, "", "", errors.New("invalid VLESS endpoint")
	}
	srv["address"] = ip
	if tr == "GRPC" {
		if err := validateGRPCUsers(srv); err != nil {
			return nil, "", "", err
		}
	}
	clean := map[string]any{"tag": "cvpn-proxy", "protocol": "vless", "settings": map[string]any{"vnext": []any{srv}}, "streamSettings": map[string]any{"network": strings.ToLower(tr), "security": strings.ToLower(sec)}}
	cleanStream := clean["streamSettings"].(map[string]any)
	if sec == "REALITY" {
		if v, ok := stream["realitySettings"].(map[string]any); ok {
			cleanStream["realitySettings"] = v
		}
	} else {
		tlsConfig, err := safeGrpcTLS(stream["tlsSettings"])
		if err != nil {
			return nil, "", "", err
		}
		cleanStream["tlsSettings"] = tlsConfig
	}
	if tr == "GRPC" {
		grpc, err := safeGrpcJSON(stream["grpcSettings"])
		if err != nil {
			return nil, "", "", err
		}
		cleanStream["grpcSettings"] = grpc
	} else if tr == "XHTTP" {
		if v, ok := stream["xhttpSettings"]; ok {
			cleanStream["xhttpSettings"] = v
		}
	} else if tr == "TCP" {
		if v, ok := stream["tcpSettings"]; ok {
			cleanStream["tcpSettings"] = v
		}
	}
	return clean, ip, "Импорт JSON", nil
}
func profileOutbound(secret string) (map[string]any, string, string, error) {
	s := strings.TrimSpace(secret)
	if strings.HasPrefix(strings.ToLower(s), "vless://") {
		return makeVLESS(s)
	}
	if strings.HasPrefix(s, "{") {
		return makeJSON(s)
	}
	return nil, "", "", errors.New("only vless:// or JSON available; subscription URLs are coming in the next update")
}
func configFor(out map[string]any) ([]byte, error) {
	m, e := readConfig()
	if e != nil {
		return nil, e
	}
	arr, _ := m["outbounds"].([]any)
	found := false
	for i, x := range arr {
		v, _ := x.(map[string]any)
		if v["protocol"] == "vless" {
			arr[i] = out
			found = true
		}
	}
	if !found {
		return nil, errors.New("trusted configuration template missing VLESS outbound")
	}
	// Reject unsafe template changes: ensure TUN and DNS interception remain as previously verified.
	ins, _ := m["inbounds"].([]any)
	tunOK := false
	for _, i := range ins {
		it, _ := i.(map[string]any)
		if it["protocol"] == "tun" {
			cfg, _ := it["settings"].(map[string]any)
			if cfg["name"] == "cvpn0" && cfg["autoOutboundsInterface"] == "eth0" {
				tunOK = true
			}
		}
	}
	if !tunOK {
		return nil, errors.New("trusted TUN setup missing")
	}
	return json.MarshalIndent(m, "", "  ")
}
func testConfig(data []byte) error {
	dir := path("webui", "staging")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	name := filepath.Join(dir, "test-"+randomID()+".json")
	defer os.Remove(name)
	if e := atomic(name, data, 0600); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path("bin", "xray"), "run", "-test", "-c", name)
	if out, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("Xray config rejected: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Subscription downloads accept HTTPS only, with no redirects and no requests to
// local/private/link-local addresses (including DNS rebinding during dial).
func fetchSubscriptionInfo(raw string) ([]string, string, string, *SubscriptionMeta, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, "", "", nil, errors.New("subscription must be an HTTPS URL")
	}
	guardDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicDestination(ip.IP) {
				continue
			}
			return (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.IP.String(), port))
		}
		return nil, errors.New("subscription resolves to a blocked local address")
	}
	tr := &http.Transport{Proxy: nil, DialContext: guardDial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 7 * time.Second, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not allowed") }}
	req, e := http.NewRequest("GET", raw, nil)
	if e != nil {
		return nil, "", "", nil, e
	}
	req.Header.Set("User-Agent", "CVPN-webOS/1.6")
	resp, e := client.Do(req)
	if e != nil {
		return nil, "", "", nil, fmt.Errorf("subscription download failed: %v", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", "", nil, fmt.Errorf("subscription HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 350001))
	if e != nil {
		return nil, "", "", nil, e
	}
	if len(b) > 350000 {
		return nil, "", "", nil, errors.New("subscription too large")
	}
	rawBody := strings.TrimSpace(string(b))
	if !strings.Contains(rawBody, "vless://") {
		compact := strings.Join(strings.Fields(rawBody), "")
		var dec []byte
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if x, err := enc.DecodeString(compact); err == nil {
				dec = x
				break
			}
		}
		if len(dec) > 0 {
			rawBody = string(dec)
		}
	}
	result := []string{}
	scanner := bufio.NewScanner(strings.NewReader(rawBody))
	scanner.Buffer(make([]byte, 2048), 350000)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(strings.ToLower(line), "vless://") {
			result = append(result, line)
		}
	}
	if len(result) == 0 {
		return nil, "", "", nil, errors.New("subscription contains no VLESS links")
	}
	if len(result) > 30 {
		return nil, "", "", nil, errors.New("subscription has more than 30 VLESS links")
	}
	title := strings.TrimSpace(resp.Header.Get("profile-title"))
	if title == "" {
		title = strings.TrimSpace(resp.Header.Get("x-profile-title"))
	}
	if strings.HasPrefix(strings.ToLower(title), "base64:") {
		v := strings.TrimSpace(title[7:])
		if dec, err := base64.StdEncoding.DecodeString(v); err == nil {
			title = string(dec)
		}
	}
	title = safeLabel(title, 60)
	return result, title, u.Hostname(), parseCVPNSubscriptionMeta(raw, resp.Header, rawBody), nil
}
func fetchSubscription(raw string) ([]string, error) {
	links, _, _, _, e := fetchSubscriptionInfo(raw)
	return links, e
}
func (a *app) importProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if e := decodeJSON(r, &req); e != nil {
		errorOut(w, 400, "Некорректный запрос")
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if len(req.Content) == 0 || len(req.Content) > 120000 {
		errorOut(w, 400, "Укажите ссылку или JSON до 120 КБ")
		return
	}
	mx.Lock()
	defer mx.Unlock()
	if active() {
		errorOut(w, 409, "Сначала отключите VPN для импорта")
		return
	}
	sub := strings.HasPrefix(strings.ToLower(req.Content), "https://")
	lines := []string{req.Content}
	title, domain := "", ""
	var subMeta *SubscriptionMeta
	if sub {
		var err error
		lines, title, domain, subMeta, err = subscriptionFetcher(req.Content)
		if err != nil {
			errorOut(w, 400, safeError(err))
			return
		}
	}
	p, e := readProfiles()
	if e != nil {
		errorOut(w, 500, "Ошибка чтения списка профилей")
		return
	}
	p, e = migrateGroups(p)
	if e != nil {
		errorOut(w, 500, "Ошибка миграции групп")
		return
	}
	groupID := "manual"
	groupName := "Отдельные подключения"
	if sub {
		groupID = subscriptionID(req.Content)
		if existing := getGroup(p, groupID); existing != nil {
			jsonResponse(w, 200, map[string]any{"ok": true, "alreadyExists": true, "imported": 0, "skipped": 0, "group": existing.Name,
				"message": "Эта подписка уже добавлена. Используйте «Обновить» в меню группы."})
			return
		}
		groupName = detectSubscriptionName(req.Name, title, lines, domain)
	}
	if !sub && strings.TrimSpace(req.Name) != "" {
		groupName = safeLabel(req.Name, 80)
	}
	if getGroup(p, groupID) == nil {
		kind := "manual"
		if sub {
			kind = "subscription"
		}
		p.Groups = append(p.Groups, Group{ID: groupID, Name: groupName, Kind: kind, Domain: domain, UpdatedAt: time.Now().UTC().Format(time.RFC3339), Meta: subMeta})
	}
	existingFingerprints := map[string]int{}
	for i, item := range p.Items {
		// De-duplicate within the destination group. Never silently move a
		// previously imported profile out of another subscription.
		if item.GroupID != groupID {
			continue
		}
		fp := fingerprintForProfile(item)
		if fp != "" {
			existingFingerprints[fp] = i
		}
	}
	imported, skipped := 0, 0
	createdSecrets := []string{}
	committed := false
	defer func() {
		if !committed {
			for _, id := range createdSecrets {
				if safeID.MatchString(id) {
					_ = os.Remove(path("webui", "secrets", id+".txt"))
				}
			}
		}
	}()
	issues := []ImportIssue{}
	for i, line := range lines {
		fp := secretFingerprint(line)
		if idx, found := existingFingerprints[fp]; found {
			skipped++
			issues = append(issues, newImportIssue(i, line, duplicateProfileReason(p.Items[idx])))
			continue
		}
		out, ip, fallback, err := profileOutbound(line)
		if err != nil {
			skipped++
			issues = append(issues, newImportIssue(i, line, importFailureReason(err)))
			continue
		}
		cfg, err := configFor(out)
		if err == nil {
			err = testConfig(cfg)
		}
		if err != nil {
			skipped++
			issues = append(issues, newImportIssue(i, line, importFailureReason(err)))
			continue
		}
		_, tr, sec, err := metaFromOutbound(out)
		if err != nil {
			skipped++
			issues = append(issues, newImportIssue(i, line, importFailureReason(err)))
			continue
		}
		name := fallback
		if !sub && strings.TrimSpace(req.Name) != "" {
			name = safeLabel(req.Name, 80)
		}
		id := randomID()
		if err = atomic(path("webui", "secrets", id+".txt"), []byte(line), 0600); err != nil {
			errorOut(w, 500, "Не удалось сохранить конфигурацию")
			return
		}
		createdSecrets = append(createdSecrets, id)
		p.Items = append(p.Items, Profile{ID: id, Name: safeLabel(name, 80), Address: ip, Transport: tr, Security: sec,
			Source: map[bool]string{true: "subscription", false: "import"}[sub], GroupID: groupID, Fingerprint: fp})
		existingFingerprints[fp] = len(p.Items) - 1
		imported++
	}
	if imported == 0 && skipped == 0 {
		errorOut(w, 400, "Нет подходящих VLESS TCP/XHTTP/gRPC конфигураций")
		return
	}
	count := 0
	for _, v := range p.Items {
		if v.GroupID == groupID {
			count++
		}
	}
	if count == 0 {
		jsonResponse(w, 422, map[string]any{"ok": false, "error": summarizeImportFailure(issues), "issues": issues})
		return
	}
	if sub {
		if err := atomic(path("webui", "secrets", "subscription-"+groupID+".txt"), []byte(req.Content), 0600); err != nil {
			errorOut(w, 500, "Не удалось сохранить источник подписки")
			return
		}
	}
	if e = writeProfiles(p); e != nil {
		errorOut(w, 500, "Не удалось обновить список профилей")
		return
	}
	committed = true
	jsonResponse(w, 200, map[string]any{"ok": true, "received": len(lines), "imported": imported, "skipped": skipped, "issues": issues, "group": groupName})
}

func (a *app) selectProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if e := decodeJSON(r, &req); e != nil {
		errorOut(w, 400, "Некорректный запрос")
		return
	}
	mx.Lock()
	defer mx.Unlock()
	if active() {
		errorOut(w, 409, "Сначала отключите VPN для изменения выбранного профиля")
		return
	}
	p, e := readProfiles()
	if e != nil {
		errorOut(w, 500, "Ошибка чтения профилей")
		return
	}
	if err := selectProfileLocked(&p, req.ID); err != nil {
		errorOut(w, 409, safeError(err))
		return
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "active": p.Active})
}
func selectProfileLocked(p *profilesFile, id string) error {
	if active() {
		return errors.New("Активное VPN-соединение: сначала отключите его")
	}
	release, lockErr := reserveEngineConfig()
	if lockErr != nil {
		return errors.New("Другая операция с VPN выполняется. Повторите позже")
	}
	defer release()
	if active() {
		return errors.New("Параллельно запустился VPN. Изменения отменены")
	}
	found := false
	for _, item := range p.Items {
		if item.ID == id {
			found = true
			break
		}
	}
	if !found {
		return errors.New("Профиль не найден")
	}
	if id == p.Active {
		return nil
	}
	var cfg, ip []byte
	if id == "installed" {
		var e, ee error
		cfg, e = os.ReadFile(path("webui", "original-client.json"))
		ip, ee = os.ReadFile(path("webui", "original-server-ip.txt"))
		if e != nil || ee != nil {
			return errors.New("Резервная копия исходного профиля недоступна")
		}
	} else {
		if !safeID.MatchString(id) {
			return errors.New("Некорректный ID профиля")
		}
		secret, e := os.ReadFile(path("webui", "secrets", id+".txt"))
		if e != nil {
			return errors.New("Файл конфигурации не найден")
		}
		out, server, _, e := profileOutbound(string(secret))
		if e != nil {
			return errors.New("Некорректные параметры профиля")
		}
		cfg, e = configFor(out)
		if e != nil {
			return e
		}
		ip = []byte(server + "\n")
	}
	if e := testConfig(cfg); e != nil {
		return errors.New("Xray отклонил параметры сервера")
	}
	oldCfg, cfgReadErr := os.ReadFile(path("config", "client.json"))
	oldIP, ipReadErr := os.ReadFile(path("config", "server-ip.txt"))
	if e := applyConfig(cfg, ip); e != nil {
		return errors.New("Не удалось применить конфигурацию")
	}
	old := p.Active
	p.Active = id
	if e := writeProfiles(*p); e != nil {
		p.Active = old
		if cfgReadErr == nil && ipReadErr == nil {
			_ = applyConfig(oldCfg, oldIP)
		} else if errors.Is(cfgReadErr, os.ErrNotExist) && errors.Is(ipReadErr, os.ErrNotExist) {
			_ = os.Remove(path("config", "client.json"))
			_ = os.Remove(path("config", "server-ip.txt"))
		}
		return errors.New("Не удалось сохранить выбранный сервер")
	}
	return nil
}

func applyConfig(cfg, server []byte) error {
	configFile := path("config", "client.json")
	ipFile := path("config", "server-ip.txt")
	oldCfg, cfgErr := os.ReadFile(configFile)
	oldIP, ipErr := os.ReadFile(ipFile)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		return cfgErr
	}
	if ipErr != nil && !errors.Is(ipErr, os.ErrNotExist) {
		return ipErr
	}
	// Never silently repair only half a previously committed configuration.
	if (cfgErr == nil) != (ipErr == nil) {
		return errors.New("incomplete existing VPN configuration; manual recovery required")
	}
	if e := atomic(configFile, cfg, 0600); e != nil {
		return e
	}
	if e := atomic(ipFile, server, 0600); e != nil {
		if cfgErr == nil {
			_ = atomic(configFile, oldCfg, 0600)
		} else {
			_ = os.Remove(configFile)
		}
		return e
	}
	_ = oldIP
	return nil
}
func bootstrap() error {
	if e := os.MkdirAll(path("webui", "secrets"), 0700); e != nil {
		return e
	}
	p, e := readProfiles()
	if e != nil {
		return e
	}
	if _, e := os.Stat(path("webui", "profiles.json")); e == nil {
		updated, err := migrateGroups(p)
		if err != nil {
			return err
		}
		if len(p.Groups) == 0 && len(updated.Groups) > 0 {
			old, _ := os.ReadFile(path("webui", "profiles.json"))
			backup := path("webui", "profiles.before-connect.json")
			if _, e := os.Stat(backup); os.IsNotExist(e) {
				if e = atomic(backup, old, 0600); e != nil {
					return e
				}
			}
			return writeProfiles(updated)
		}
		return nil
	}
	if existing, e := readExisting(); e == nil {
		b, ee := os.ReadFile(path("config", "client.json"))
		s, ee2 := os.ReadFile(path("config", "server-ip.txt"))
		if ee == nil && ee2 == nil {
			if e := atomic(path("webui", "original-client.json"), b, 0600); e != nil {
				return e
			}
			if e := atomic(path("webui", "original-server-ip.txt"), s, 0600); e != nil {
				return e
			}
			p.Active = "installed"
			existing.GroupID = "installed"
			p.Items = append(p.Items, existing)
			p.Groups = append(p.Groups, Group{ID: "installed", Name: "Локальные профили", Kind: "installed"})
		}
	}
	return writeProfiles(p)
}
func main() {
	flag.StringVar(&base, "base", base, "CVPN engine directory")
	flag.StringVar(&address, "listen", address, "web server bind address")
	flag.Parse()
	if e := bootstrap(); e != nil {
		log.Fatal(e)
	}
	startGeoIPAutoUpdater()
	runBootAutoConnect()
	keyBytes, err := os.ReadFile(path("webui", "access.key"))
	fail(err)
	key := strings.TrimSpace(string(keyBytes))
	if len(key) < 28 {
		log.Fatal("access key is too short")
	}
	a := &app{key: key}
	sub, e := fs.Sub(frontend, "public")
	fail(e)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", a.api)
	mux.HandleFunc("/tv", a.tvPairingPage)
	mux.HandleFunc("/tv/notify", a.tvNativeNotify)
	mux.HandleFunc("/tv/qr-data.js", a.tvQRData)
	mux.HandleFunc("/tv/language.js", a.tvLanguageData)
	fileserver := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'")
		}
		fileserver.ServeHTTP(w, r)
	})
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "unrecognized client", 403)
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
			http.Error(w, "LAN access only", 403)
			return
		}
		if !localClientAllowed(ip) {
			http.Error(w, "outside permitted LAN subnet", 403)
			return
		}
		if !localTVRequest(r) && r.Header.Get("X-CVPN-Key") != "" {
			http.Error(w, "legacy admin header is loopback-only", 403)
			return
		}
		if !allowedHost(r.Host) {
			http.Error(w, "invalid host", 403)
			return
		}
		mux.ServeHTTP(w, r)
	})
	server := http.Server{Addr: address, Handler: guarded, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	log.Printf("CVPN webOS panel %s listening on %s", version, address)
	// Notifications are advisory. The monitor never changes routes or engine state.
	recordStartupObservation()
	go watchNetworkHealth(context.Background())
	go watchVPNEvents(context.Background())
	fail(server.ListenAndServe())
}

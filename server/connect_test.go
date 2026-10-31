package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepareFixture(t *testing.T) string {
	t.Helper()
	originalProbe := runtimeProbe
	runtimeProbe = func() RuntimeHealth { return RuntimeHealth{Healthy: true} }
	t.Cleanup(func() { runtimeProbe = originalProbe })
	tmp := t.TempDir()
	for _, d := range []string{"config", "bin", "state", "webui", "webui/secrets"} {
		if e := os.MkdirAll(filepath.Join(tmp, d), 0700); e != nil {
			t.Fatal(e)
		}
	}
	template := `{"log":{"loglevel":"info"},"inbounds":[{"protocol":"tun","tag":"cvpn-tun","settings":{"name":"cvpn0","autoOutboundsInterface":"eth0"}}],"outbounds":[{"protocol":"vless","tag":"cvpn-proxy","settings":{"vnext":[{"address":"1.1.1.1","port":443,"users":[{"id":"11111111-1111-1111-1111-111111111111","encryption":"none"}]}]},"streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"serverName":"www.google.com","password":"test","fingerprint":"chrome"},"xhttpSettings":{"path":"/","mode":"packet-up"}}}]}`
	if e := os.WriteFile(filepath.Join(tmp, "config", "client.json"), []byte(template), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(tmp, "config", "server-ip.txt"), []byte("1.1.1.1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	script := `#!/bin/sh
case "$1" in
 start) echo ready > "` + tmp + `/state/active";echo ready > "` + tmp + `/state/phase";echo trial > "` + tmp + `/state/mode";echo 'VPN_CONNECTED=YES';;
 stop) rm -f "` + tmp + `/state/active";echo 'DNS_MOUNT_RESTORED=YES'; echo 'NETWORK_RESTORED=YES';;
 confirm) echo confirmed > "` + tmp + `/state/mode";;
esac
`
	for n, s := range map[string]string{"cvpn.sh": script, "xray": "#!/bin/sh\necho Configuration OK\n"} {
		fn := filepath.Join(tmp, "bin", n)
		if e := os.WriteFile(fn, []byte(s), 0700); e != nil {
			t.Fatal(e)
		}
	}
	return tmp
}
func TestDetectSubscriptionName(t *testing.T) {
	a := "vless://11111111-1111-1111-1111-111111111111@1.1.1.1:443?security=reality&type=xhttp&sni=www.google.com&pbk=abc#CVPN%20%C2%B7%20NL"
	b := strings.Replace(a, "NL", "DE", 1)
	cases := []struct {
		title, domain, expect string
		lines                 []string
	}{{"", "cvpn.su", "CVPN", []string{a, b}}, {"", "vpn-example.io", "VжN", nil}, {"Friendly Name", "cvpn.su", "Friendly Name", []string{a, b}}}
	for _, c := range cases {
		if len(c.lines) == 0 {
			continue
		}
		got := detectSubscriptionName("", c.title, c.lines, c.domain)
		if got != c.expect {
			t.Fatalf("got %q want %q", got, c.expect)
		}
	}
	if x := detectSubscriptionName("", "", []string{"vless://x#NL"}, "cvpn.su"); x != "CVPN" {
		t.Fatal(x)
	}
}
func TestImportDedupAndGroups(t *testing.T) {
	prev := base
	base = prepareFixture(t)
	defer func() { base = prev }()
	if e := bootstrap(); e != nil {
		t.Fatal(e)
	}
	a := &app{key: strings.Repeat("z", 40)}
	content := "vless://22222222-2222-2222-2222-222222222222@1.1.1.1:443?security=reality&type=xhttp&sni=www.google.com&pbk=test&path=%2F&mode=packet-up#CVPN%20%C2%B7%20NL"
	post := func(endpoint, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", endpoint, strings.NewReader(body))
		r.Header.Set("X-CVPN-Key", a.key)
		r.RemoteAddr = "127.0.0.1:12345"
		a.api(w, r)
		return w
	}
	input, _ := json.Marshal(map[string]string{"content": content})
	first := post("/api/import", string(input))
	if first.Code != 200 {
		t.Fatalf("first %v %s", first.Code, first.Body.String())
	}
	second := post("/api/import", string(input))
	if second.Code == 200 { // already-present direct link should never duplicate
	} else if second.Code != 400 {
		t.Fatalf("duplicate status %d", second.Code)
	}
	p, e := readProfiles()
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 2 {
		t.Fatalf("duplicate import created item: %d", len(p.Items))
	}
	got := post("/api/connect", `{"id":"`+p.Items[1].ID+`"}`)
	if got.Code != 200 {
		t.Fatalf("connect %d: %s", got.Code, got.Body.String())
	}
	got = post("/api/connect", `{"id":"`+p.Items[1].ID+`"}`)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "unchanged") {
		t.Fatalf("repeated tap should not disconnect: %s", got.Body.String())
	}
	if !active() {
		t.Fatal("repeated tap stopped vpn")
	}
	got = post("/api/connect", `{"id":"installed"}`)
	if got.Code != 200 {
		t.Fatalf("switch: %d: %s", got.Code, got.Body.String())
	}
	if !active() {
		t.Fatal("switch should connect old profile")
	}
	got = post("/api/groups/delete", `{"id":"manual"}`)
	if got.Code != 409 {
		t.Fatalf("delete active group must fail: %d", got.Code)
	}
}

func TestAutoRecoveryOnConnect(t *testing.T) {
	oldBase := base
	base = prepareFixture(t)
	defer func() { base = oldBase }()
	if err := bootstrap(); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"active": "session", "phase": "ready", "mode": "persistent"} {
		if err := os.WriteFile(path("state", k), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unhealthy := true
	runtimeProbe = func() RuntimeHealth {
		if unhealthy {
			return RuntimeHealth{Healthy: false, Reason: "Missing TUN"}
		}
		return RuntimeHealth{Healthy: true}
	}
	if !status()["needsRecovery"].(bool) {
		t.Fatal("expected needsRecovery before connect")
	}
	a := &app{key: strings.Repeat("z", 40)}
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-CVPN-Key", a.key)
		r.RemoteAddr = "127.0.0.1:12345"
		a.api(w, r)
		return w
	}
	unhealthy = false
	got := post("/api/connect", `{"id":"installed"}`)
	if got.Code != 200 {
		t.Fatalf("connect failed during auto-recovery: %d: %s", got.Code, got.Body.String())
	}
	if !active() {
		t.Fatal("expected active VPN after auto-recovery connect")
	}
	if status()["needsRecovery"] == true {
		t.Fatal("did not expect needsRecovery after auto-recovery connect")
	}
}

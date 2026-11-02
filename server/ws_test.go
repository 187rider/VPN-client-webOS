package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const wsUUID = "d6257a76-4406-425b-8e30-079766220164"
const wsIP = "104.19.49.146"

func wsURI(params string) string {
	return "vless://" + wsUUID + "@" + wsIP + ":443?type=ws&" + params + "#WS%20test"
}

func TestWSTLSLink(t *testing.T) {
	link := wsURI("security=tls&host=bus9api.ridertech.uk&fp=chrome&sni=bus9api.ridertech.uk&path=%2Fapi%2Fhttp%3A%2F%2F8MfiToqgMt%3AYXVFuCb66N%40192.227.193.202%3A8081&encryption=none&alpn=")
	out, host, label, err := makeVLESS(link)
	if err != nil {
		t.Fatal(err)
	}
	if host != wsIP || label != "WS test" {
		t.Fatalf("%q %q", host, label)
	}
	_, tr, sec, err := metaFromOutbound(out)
	if err != nil || tr != "WS" || sec != "TLS" {
		t.Fatalf("%q %q %v", tr, sec, err)
	}
	stream := out["streamSettings"].(map[string]any)
	tls := stream["tlsSettings"].(map[string]any)
	if stream["security"] != "tls" || tls["serverName"] != "bus9api.ridertech.uk" || tls["fingerprint"] != "chrome" {
		t.Fatalf("bad tls %#v", stream)
	}
	if _, hasAlpn := tls["alpn"]; hasAlpn {
		t.Fatalf("expected empty alpn to be omitted, got %#v", tls["alpn"])
	}
	ws := stream["wsSettings"].(map[string]any)
	if ws["path"] != "/api/http://8MfiToqgMt:YXVFuCb66N@192.227.193.202:8081" {
		t.Fatalf("bad ws path %#v", ws)
	}
	headers := ws["headers"].(map[string]string)
	if headers["Host"] != "bus9api.ridertech.uk" {
		t.Fatalf("bad ws headers %#v", headers)
	}
	if _, exists := stream["grpcSettings"]; exists {
		t.Fatal("unexpected gRPC config")
	}
	if _, exists := stream["xhttpSettings"]; exists {
		t.Fatal("unexpected XHTTP config")
	}
	cfg, err := json.Marshal(out)
	if err != nil || !strings.Contains(string(cfg), `"wsSettings"`) {
		t.Fatal(err)
	}
}

func TestWSRejectsBadLinks(t *testing.T) {
	for _, q := range []string{
		"security=tls&sni=example.com&flow=xtls-rprx-vision",
		"security=tls&sni=example.com&allowInsecure=1",
		"security=tls&sni=example.com&insecure=true",
		"security=tls&sni=example.com&path=%0A",
		"security=none&path=/",
	} {
		if _, _, _, err := makeVLESS(wsURI(q)); err == nil {
			t.Fatalf("unsafe WebSocket accepted: %q", q)
		}
	}
}

func TestWSJSONImports(t *testing.T) {
	raw := `{"protocol":"vless","settings":{"vnext":[{"address":"104.19.49.146","port":443,"users":[{"id":"` + wsUUID + `","encryption":"none"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"bus9api.ridertech.uk","fingerprint":"chrome"},"wsSettings":{"path":"/api","headers":{"Host":"bus9api.ridertech.uk"}}}}`
	out, ip, label, err := makeJSON(raw)
	if err != nil {
		t.Fatalf("makeJSON failed: %v", err)
	}
	if ip != wsIP || label != "Импорт JSON" {
		t.Fatalf("%q %q", ip, label)
	}
	_, tr, sec, err := metaFromOutbound(out)
	if err != nil || tr != "WS" || sec != "TLS" {
		t.Fatalf("%q %q %v", tr, sec, err)
	}
	stream := out["streamSettings"].(map[string]any)
	ws := stream["wsSettings"].(map[string]any)
	if ws["path"] != "/api" {
		t.Fatalf("bad ws path %#v", ws)
	}
}

func TestWSJSONRejectsInsecureAndFlow(t *testing.T) {
	for _, inject := range []string{`"allowInsecure":true`, `"allowInsecure":"false"`} {
		raw := `{"protocol":"vless","settings":{"vnext":[{"address":"104.19.49.146","port":443,"users":[{"id":"` + wsUUID + `","encryption":"none"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"example.org",` + inject + `},"wsSettings":{"path":"/"}}}`
		if _, _, _, err := makeJSON(raw); err == nil {
			t.Fatalf("unsafe WS TLS accepted: %s", inject)
		}
	}
	flowRaw := `{"protocol":"vless","settings":{"vnext":[{"address":"104.19.49.146","port":443,"users":[{"id":"` + wsUUID + `","encryption":"none","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"example.org"},"wsSettings":{"path":"/"}}}`
	if _, _, _, err := makeJSON(flowRaw); err == nil {
		t.Fatalf("WS with flow accepted: %s", flowRaw)
	}
}

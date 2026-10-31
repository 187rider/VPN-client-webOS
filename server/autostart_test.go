package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAutostartConfigReadWrite(t *testing.T) {
	setupTestBase(t)

	cfg := readAutostartConfig()
	if cfg.VPNAutoConnect {
		t.Fatal("expected VPNAutoConnect false by default")
	}
	if cfg.DelaySeconds != 15 {
		t.Fatalf("expected default 15s delay, got %d", cfg.DelaySeconds)
	}
	if !cfg.NotifyOnStartup {
		t.Fatal("expected NotifyOnStartup true by default")
	}

	cfg.VPNAutoConnect = true
	cfg.DelaySeconds = 25
	cfg.NotifyOnStartup = false
	if err := writeAutostartConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cfg2 := readAutostartConfig()
	if !cfg2.VPNAutoConnect {
		t.Fatal("expected VPNAutoConnect true")
	}
	if cfg2.DelaySeconds != 25 {
		t.Fatalf("expected 25s, got %d", cfg2.DelaySeconds)
	}
	if cfg2.NotifyOnStartup {
		t.Fatal("expected NotifyOnStartup false")
	}
}

func TestAutostartAPI(t *testing.T) {
	setupTestBase(t)

	p := profilesFile{Active: "node1", Items: []Profile{{ID: "node1", Name: "Server DE", Address: "1.2.3.4"}}}
	if err := writeProfiles(p); err != nil {
		t.Fatal(err)
	}

	key := strings.Repeat("k", 48)
	a := &app{key: key}

	// 1. GET /api/autostart
	req := httptest.NewRequest(http.MethodGet, "/api/autostart", nil)
	req.Header.Set("X-CVPN-Key", key)
	req.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	a.api(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var statusResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &statusResp); err != nil {
		t.Fatal(err)
	}
	if statusResp["vpnAutoConnect"] != false {
		t.Fatal("expected false by default")
	}
	if statusResp["activeProfile"] != "Server DE" || statusResp["activeProfileIp"] != "1.2.3.4" {
		t.Fatalf("unexpected profile info: %#v", statusResp)
	}
	if _, ok := statusResp["webAddress"]; !ok {
		t.Fatal("expected webAddress in autostart status response")
	}

	// 2. POST /api/autostart
	body := []byte(`{"vpnAutoConnect":true,"delaySeconds":20,"panelHookEnabled":true,"notifyOnStartup":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/autostart", bytes.NewReader(body))
	req.Header.Set("X-CVPN-Key", key)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	a.api(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var saveResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &saveResp); err != nil {
		t.Fatal(err)
	}
	if saveResp["vpnAutoConnect"] != true {
		t.Fatal("expected vpnAutoConnect true after save")
	}
	if saveResp["notifyOnStartup"] != true {
		t.Fatal("expected notifyOnStartup true")
	}
}

func TestStartupNotificationWithWebAddress(t *testing.T) {
	setupTestBase(t)

	var intercepted []string
	prev := notifySystemToast
	notifySystemToast = func(msg string) error {
		intercepted = append(intercepted, msg)
		return nil
	}
	t.Cleanup(func() { notifySystemToast = prev })

	sendStartupStatusToast("test-node", "10.0.0.1", true, nil)
	if len(intercepted) == 0 {
		t.Fatal("expected at least 1 notification message intercepted")
	}
	last := intercepted[len(intercepted)-1]
	if !strings.Contains(last, "test-node") || !strings.Contains(last, "10.0.0.1") {
		t.Fatalf("missing node info in toast: %q", last)
	}
	if !strings.Contains(last, ":8765") {
		t.Fatalf("missing webserver port/address in toast: %q", last)
	}
}

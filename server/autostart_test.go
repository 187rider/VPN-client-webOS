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

	cfg.VPNAutoConnect = true
	cfg.DelaySeconds = 25
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
}

func TestAutostartAPI(t *testing.T) {
	setupTestBase(t)

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

	// 2. POST /api/autostart
	body := []byte(`{"vpnAutoConnect":true,"delaySeconds":20,"panelHookEnabled":true}`)
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
	if saveResp["delaySeconds"] != float64(20) {
		t.Fatalf("expected 20, got %v", saveResp["delaySeconds"])
	}
}

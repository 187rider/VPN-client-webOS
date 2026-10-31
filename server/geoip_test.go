package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func setupTestBase(t *testing.T) string {
	d := t.TempDir()
	orig := base
	base = d
	t.Cleanup(func() { base = orig })
	_ = os.MkdirAll(path("bin"), 0700)
	_ = os.MkdirAll(path("webui"), 0700)
	return d
}

func TestGeoIPConfigReadWrite(t *testing.T) {
	setupTestBase(t)

	cfg := readGeoIPConfig()
	if !cfg.AutoUpdate {
		t.Fatal("expected autoUpdate to be true by default")
	}
	if cfg.UpdateIntervalDays != 7 {
		t.Fatalf("expected 7 days by default, got %d", cfg.UpdateIntervalDays)
	}
	if cfg.GeoIPURL != defaultGeoIPURL {
		t.Fatalf("unexpected default geoip url %q", cfg.GeoIPURL)
	}

	cfg.AutoUpdate = false
	cfg.UpdateIntervalDays = 3
	cfg.GeoIPURL = "https://custom.example.com/geoip.dat"
	cfg.GeoSiteURL = "https://custom.example.com/geosite.dat"
	if err := writeGeoIPConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cfg2 := readGeoIPConfig()
	if cfg2.AutoUpdate {
		t.Fatal("expected autoUpdate to be false")
	}
	if cfg2.UpdateIntervalDays != 3 {
		t.Fatalf("expected 3 days, got %d", cfg2.UpdateIntervalDays)
	}
	if cfg2.GeoIPURL != "https://custom.example.com/geoip.dat" {
		t.Fatalf("unexpected geoip url %q", cfg2.GeoIPURL)
	}
}

func TestGeoIPStatusAndSettingsAPI(t *testing.T) {
	setupTestBase(t)

	// Create dummy geoip.dat and geosite.dat in bin
	_ = os.WriteFile(path("bin", "geoip.dat"), []byte("geoip test content"), 0644)
	_ = os.WriteFile(path("bin", "geosite.dat"), []byte("geosite test content"), 0644)

	key := strings.Repeat("k", 48)
	a := &app{key: key}

	// 1. GET /api/geoip/status
	req := httptest.NewRequest(http.MethodGet, "/api/geoip/status", nil)
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
	if statusResp["enabled"] != true {
		t.Fatal("expected enabled to be true")
	}
	if statusResp["autoUpdate"] != true {
		t.Fatal("expected autoUpdate to be true")
	}

	// 2. POST /api/geoip/settings with custom config
	body := []byte(`{"autoUpdate":false,"updateIntervalDays":14,"geoipUrl":"https://example.com/ip.dat","geositeUrl":"https://example.com/site.dat"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/geoip/settings", bytes.NewReader(body))
	req.Header.Set("X-CVPN-Key", key)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	a.api(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var setResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &setResp); err != nil {
		t.Fatal(err)
	}
	if setResp["autoUpdate"] != false {
		t.Fatal("expected autoUpdate to be false after settings change")
	}
	if setResp["updateIntervalDays"] != float64(14) {
		t.Fatalf("expected 14, got %v", setResp["updateIntervalDays"])
	}
}

func TestGeoIPDownloadAndValidate(t *testing.T) {
	setupTestBase(t)

	// Mock HTTP server serving large enough payload
	mockPayload := make([]byte, 600*1024) // 600 KB
	for i := range mockPayload {
		mockPayload[i] = byte(i % 256)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "small") {
			_, _ = w.Write([]byte("too small file"))
			return
		}
		_, _ = w.Write(mockPayload)
	}))
	defer srv.Close()

	// 1. Should fail if file too small
	err := downloadGeoFile(srv.URL+"/small", "geoip.dat")
	if err == nil {
		t.Fatal("expected error on small download, got nil")
	}

	// 2. Should succeed on valid payload
	err = downloadGeoFile(srv.URL+"/large.dat", "geoip.dat")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	saved, err := os.ReadFile(path("bin", "geoip.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != len(mockPayload) {
		t.Fatalf("size mismatch: expected %d, got %d", len(mockPayload), len(saved))
	}
}

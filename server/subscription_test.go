package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callAPI(a *app, endpoint string, payload any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", endpoint, strings.NewReader(string(b)))
	r.Header.Set("X-CVPN-Key", a.key)
	r.RemoteAddr = "127.0.0.1:12345"
	a.api(w, r)
	return w
}
func TestSubscriptionLifecycle(t *testing.T) {
	oldBase := base
	base = prepareFixture(t)
	defer func() { base = oldBase }()
	if err := bootstrap(); err != nil {
		t.Fatal(err)
	}
	prevFetcher := subscriptionFetcher
	defer func() { subscriptionFetcher = prevFetcher }()
	u1 := "vless://22222222-2222-2222-2222-222222222222@1.1.1.1:443?security=reality&type=xhttp&sni=www.google.com&pbk=test&mode=packet-up#CVPN%20%C2%B7%20NL"
	u2 := "vless://33333333-3333-3333-3333-333333333333@1.1.1.1:443?security=reality&type=tcp&sni=www.google.com&pbk=test&flow=xtls-rprx-vision#CVPN%20%C2%B7%20DE"
	content := []string{u1, u2}
	subscriptionFetcher = func(_ string) ([]string, string, string, *SubscriptionMeta, error) { return content, "", "cvpn.su", nil, nil }
	a := &app{key: strings.Repeat("x", 40)}
	source := "https://cvpn.su/sub/private-token-never-expose"
	resp := callAPI(a, "/api/import", map[string]any{"content": source})
	if resp.Code != 200 {
		t.Fatalf("import error %d %s", resp.Code, resp.Body.String())
	}
	p, e := readProfiles()
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 3 || len(p.Groups) != 2 {
		t.Fatalf("unexpected counts: %d / %d", len(p.Items), len(p.Groups))
	}
	gid := subscriptionID(source)
	if p.Groups[1].Name != "CVPN" {
		t.Fatalf("automatic name %q", p.Groups[1].Name)
	}
	if s, _ := os.ReadFile(filepath.Join(base, "webui", "secrets", "subscription-"+gid+".txt")); strings.TrimSpace(string(s)) != source {
		t.Fatal("source not securely stored")
	}
	b, _ := json.Marshal(publicList(p))
	if strings.Contains(string(b), "private-token") {
		t.Fatal("secret leaked into public API")
	}
	resp = callAPI(a, "/api/import", map[string]any{"content": source})
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), "alreadyExists") {
		t.Fatalf("repeat import %d %s", resp.Code, resp.Body.String())
	}
	resp = callAPI(a, "/api/groups/rename", map[string]any{"id": gid, "name": "My VPN"})
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	p, _ = readProfiles()
	first := p.Items[1].ID
	second := p.Items[2].ID
	resp = callAPI(a, "/api/select", map[string]any{"id": second})
	if resp.Code != 200 {
		t.Fatalf("select failed: %s", resp.Body.String())
	}
	content = []string{u2}
	resp = callAPI(a, "/api/groups/refresh", map[string]any{"id": gid})
	if resp.Code != 200 {
		t.Fatalf("refresh failed %d %s", resp.Code, resp.Body.String())
	}
	p, _ = readProfiles()
	if len(p.Items) != 2 || p.Active != second || p.Groups[1].Name != "My VPN" {
		t.Fatalf("refresh did not preserve selected server/name: %+v", p)
	}
	if _, e = os.Stat(filepath.Join(base, "webui", "secrets", first+".txt")); !os.IsNotExist(e) {
		t.Fatal("removed server secret persists")
	}
	resp = callAPI(a, "/api/groups/delete", map[string]any{"id": gid})
	if resp.Code != 200 {
		t.Fatalf("delete error %s", resp.Body.String())
	}
	p, _ = readProfiles()
	if len(p.Items) != 1 || p.Active != "installed" {
		t.Fatalf("delete failed fallback: %+v", p)
	}
}
func TestLegacyMigrationKeepsOriginal(t *testing.T) {
	prev := base
	base = prepareFixture(t)
	defer func() { base = prev }()
	legacy := profilesFile{Active: "installed", Items: []Profile{{ID: "installed", Name: "Установленный профиль", Source: "existing"}, {ID: "aaaaaaaaaaaaaaaaaaaaaaaa", Name: "CVPN · NL Нидерланды", Source: "subscription"}}}
	if e := writeProfiles(legacy); e != nil {
		t.Fatal(e)
	}
	if e := bootstrap(); e != nil {
		t.Fatal(e)
	}
	p, _ := readProfiles()
	if len(p.Items) != 2 || len(p.Groups) != 2 || p.Items[1].GroupID != "legacy-CVPN" {
		t.Fatalf("migration %+v", p)
	}
	if _, e := os.Stat(filepath.Join(base, "webui", "profiles.before-connect.json")); e != nil {
		t.Fatal("missing backup")
	}
}

func TestDeleteGroupWithoutInstalledProfile(t *testing.T) {
	prev := base
	base = prepareFixture(t)
	defer func() { base = prev }()
	if e := bootstrap(); e != nil {
		t.Fatal(e)
	}

	// Simulate setup where "installed" profile does NOT exist (e.g. only imported groups)
	u1 := "vless://22222222-2222-2222-2222-222222222222@1.1.1.1:443?security=reality&type=xhttp&sni=www.google.com&pbk=test&mode=packet-up#Sub1%20Node"
	content := []string{u1}
	prevFetcher := subscriptionFetcher
	defer func() { subscriptionFetcher = prevFetcher }()
	subscriptionFetcher = func(_ string) ([]string, string, string, *SubscriptionMeta, error) { return content, "", "example.com", nil, nil }

	a := &app{key: strings.Repeat("x", 40)}
	source := "https://example.com/sub/token123"
	resp := callAPI(a, "/api/import", map[string]any{"content": source})
	if resp.Code != 200 {
		t.Fatalf("import error %d %s", resp.Code, resp.Body.String())
	}

	// Remove "installed" profile completely to simulate clean setup
	p, _ := readProfiles()
	var withoutInstalled []Profile
	for _, it := range p.Items {
		if it.ID != "installed" {
			withoutInstalled = append(withoutInstalled, it)
		}
	}
	p.Items = withoutInstalled
	gid := subscriptionID(source)
	p.Active = p.Items[0].ID
	if e := writeProfiles(p); e != nil {
		t.Fatal(e)
	}

	// Delete group containing active profile when "installed" profile doesn't exist
	resp = callAPI(a, "/api/groups/delete", map[string]any{"id": gid})
	if resp.Code != 200 {
		t.Fatalf("delete failed without installed profile: %d %s", resp.Code, resp.Body.String())
	}

	p, _ = readProfiles()
	if len(p.Items) != 0 || p.Active != "" {
		t.Fatalf("expected 0 items and empty active, got %+v", p)
	}

	// Add an empty group to p.Groups (0 items in p.Items)
	p.Groups = append(p.Groups, Group{ID: "empty-group-id", Name: "Empty Group", Kind: "subscription"})
	if e := writeProfiles(p); e != nil {
		t.Fatal(e)
	}

	// Deleting an empty group should succeed without 409 "Пустая группа"
	resp = callAPI(a, "/api/groups/delete", map[string]any{"id": "empty-group-id"})
	if resp.Code != 200 {
		t.Fatalf("delete empty group failed: %d %s", resp.Code, resp.Body.String())
	}
	p, _ = readProfiles()
	for _, g := range p.Groups {
		if g.ID == "empty-group-id" {
			t.Fatal("empty-group-id should have been deleted")
		}
	}
}

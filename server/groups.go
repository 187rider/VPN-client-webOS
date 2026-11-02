package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var subscriptionFetcher = fetchSubscriptionInfo

type Group struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Domain    string            `json:"domain,omitempty"`
	UpdatedAt string            `json:"updatedAt,omitempty"`
	Meta      *SubscriptionMeta `json:"meta,omitempty"`
}

var prefixPattern = regexp.MustCompile(`(?i)^\s*([\p{L}\p{N}]{2,32})\s*[-–—·|:]\s*`)
var countryPattern = regexp.MustCompile(`(?i)^(?:EU|NL|DE|US|RU|UK|FR|SE|JP|SG)\b`)

func safeLabel(s string, n int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s))
	rs := []rune(s)
	if len(rs) > n {
		s = string(rs[:n])
	}
	return s
}
func subscriptionID(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:12])
}

// The VLESS fragment is a subscription's logical profile identity. Two
// intentionally named choices (e.g. Automatic and Germany) can resolve to the
// same current endpoint. Suppress only repeated entries with the SAME URI and
// name, never two differently named choices.
func secretFingerprint(raw string) string {
	s := strings.TrimSpace(raw)
	if u, err := url.Parse(s); err == nil && strings.EqualFold(u.Scheme, "vless") {
		// URL parsing normalizes percent-encoded display fragments, but
		// intentionally keeps the name as part of the fingerprint.
		s = u.String()
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func fingerprintForProfile(p Profile) string {
	// Older CVPN versions stored fingerprints without the fragment. Use
	// the actual stored secret to migrate the identity during refresh.
	if safeID.MatchString(p.ID) {
		if b, e := os.ReadFile(path("webui", "secrets", p.ID+".txt")); e == nil {
			return secretFingerprint(string(b))
		}
	}
	return p.Fingerprint
}

// A unique subscription label is a secondary, stable key when an automatic
// profile changes its endpoint on the next subscription refresh. This is
// never used if the same name occurs more than once in the subscription.
func subscriptionLabel(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "vless") {
		return ""
	}
	name := safeLabel(u.Fragment, 80)
	if name == "" {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}
func profileLabel(p Profile) string {
	return strings.ToLower(strings.Join(strings.Fields(safeLabel(p.Name, 80)), " "))
}
func getGroup(p profilesFile, id string) *Group {
	for _, g := range p.Groups {
		if g.ID == id {
			x := g
			return &x
		}
	}
	return nil
}
func migrateGroups(p profilesFile) (profilesFile, error) {
	if len(p.Groups) > 0 {
		for i := range p.Items {
			if p.Items[i].GroupID == "" {
				p.Items[i].GroupID = legacyGroupID(p.Items[i])
			}
		}
		return p, nil
	}
	seen := map[string]bool{}
	for i := range p.Items {
		item := &p.Items[i]
		item.GroupID = legacyGroupID(*item)
		if item.Fingerprint == "" {
			item.Fingerprint = fingerprintForProfile(*item)
		}
		if seen[item.GroupID] {
			continue
		}
		seen[item.GroupID] = true
		name := "Отдельные подключения"
		kind := "legacy"
		if item.GroupID == "installed" {
			name = "Локальные профили"
			kind = "installed"
		}
		if item.GroupID == "manual" {
			name = "Отдельные подключения"
			kind = "manual"
		}
		if strings.HasPrefix(item.GroupID, "legacy-") {
			name = strings.TrimPrefix(item.GroupID, "legacy-")
		}
		p.Groups = append(p.Groups, Group{ID: item.GroupID, Name: name, Kind: kind})
	}
	return p, nil
}
func legacyGroupID(p Profile) string {
	if p.ID == "installed" || p.Source == "existing" {
		return "installed"
	}
	if p.GroupID != "" {
		return p.GroupID
	}
	name := safeLabel(p.Name, 80)
	if m := prefixPattern.FindStringSubmatch(name); len(m) > 1 && !countryPattern.MatchString(m[1]) {
		return "legacy-" + safeLabel(m[1], 32)
	}
	if strings.HasPrefix(strings.ToUpper(name), "CVPN") {
		return "legacy-CVPN"
	}
	return "manual"
}
func detectSubscriptionName(custom, meta string, links []string, domain string) string {
	if s := safeLabel(custom, 60); s != "" {
		return s
	}
	if s := safeLabel(meta, 60); s != "" {
		return s
	}
	counts := map[string]int{}
	for _, line := range links {
		u, e := url.Parse(line)
		if e != nil {
			continue
		}
		name, _ := url.PathUnescape(u.Fragment)
		m := prefixPattern.FindStringSubmatch(name)
		if len(m) > 1 && !countryPattern.MatchString(m[1]) {
			counts[m[1]]++
		}
	}
	best, count := "", 0
	for name, n := range counts {
		if n > count || (n == count && name < best) {
			best, count = name, n
		}
	}
	if best != "" && count >= 2 && count*2 >= len(links) {
		return safeLabel(best, 60)
	}
	if domain != "" {
		host := strings.TrimPrefix(strings.ToLower(domain), "www.")
		parts := strings.Split(host, ".")
		if len(parts) >= 2 {
			label := parts[len(parts)-2]
			if len(parts) >= 3 && (parts[len(parts)-2] == "co" || parts[len(parts)-2] == "com") {
				label = parts[len(parts)-3]
			}
			if label != "" && label != "sub" && label != "api" && label != "vpn" {
				if len(label) <= 5 {
					return strings.ToUpper(label)
				}
				return strings.ToUpper(label[:1]) + label[1:]
			}
		}
	}
	return "Моя подписка"
}
func safeError(e error) string {
	if e == nil {
		return ""
	}
	// Network errors may echo secret subscription URLs; never surface them to browser.
	s := e.Error()
	if strings.Contains(s, "https://") || strings.Contains(s, "vless://") {
		return "Не удалось загрузить подписку. Проверьте ссылку, DNS и доступность сервера."
	}
	return s
}
func (a *app) groupAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name,omitempty"`
	}
	if e := decodeJSON(r, &req); e != nil {
		errorOut(w, 400, "Некорректные данные")
		return
	}
	mx.Lock()
	defer mx.Unlock()
	p, e := readProfiles()
	if e != nil {
		errorOut(w, 500, "Список профилей недоступен")
		return
	}
	p, e = migrateGroups(p)
	if e != nil {
		errorOut(w, 500, "Ошибка групп")
		return
	}
	g := getGroup(p, req.ID)
	if g == nil {
		errorOut(w, 404, "Группа не найдена")
		return
	}
	switch r.URL.Path {
	case "/api/groups/rename":
		name := safeLabel(req.Name, 60)
		if name == "" {
			errorOut(w, 400, "Введите название группы")
			return
		}
		for i := range p.Groups {
			if p.Groups[i].ID == req.ID {
				p.Groups[i].Name = name
			}
		}
		if e = writeProfiles(p); e != nil {
			errorOut(w, 500, "Ошибка сохранения")
			return
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "name": name})
	case "/api/groups/delete":
		if g.Kind == "installed" {
			errorOut(w, 409, "Исходный установленный профиль нельзя удалить")
			return
		}
		if active() {
			errorOut(w, 409, "Отключите VPN перед удалением группы")
			return
		}
		keep := make([]Profile, 0, len(p.Items))
		removed := []Profile{}
		for _, item := range p.Items {
			if item.GroupID == req.ID {
				removed = append(removed, item)
			} else {
				keep = append(keep, item)
			}
		}
		// Avoid orphaned active selection: switch to another profile or clear active
		if containsActive(removed, p.Active) {
			switched := false
			for _, candidate := range keep {
				if candidate.ID == "installed" {
					if e := selectProfileLocked(&p, "installed"); e == nil {
						switched = true
						break
					}
				}
			}
			if !switched && len(keep) > 0 {
				for _, candidate := range keep {
					if e := selectProfileLocked(&p, candidate.ID); e == nil {
						switched = true
						break
					}
				}
			}
			if !switched {
				p.Active = ""
				_ = os.Remove(path("config", "server-ip.txt"))
				if len(embeddedClientTemplate) > 0 {
					_ = atomic(path("config", "client.json"), embeddedClientTemplate, 0600)
				}
			}
		}
		p.Items = keep
		next := make([]Group, 0, len(p.Groups))
		for _, item := range p.Groups {
			if item.ID != req.ID {
				next = append(next, item)
			} else if item.ID == "manual" {
				// Keep manual group container with 0 items
				next = append(next, Group{ID: "manual", Name: item.Name, Kind: "manual"})
			}
		}
		p.Groups = next
		if e = writeProfiles(p); e != nil {
			errorOut(w, 500, "Ошибка сохранения")
			return
		}
		for _, item := range removed {
			if safeID.MatchString(item.ID) {
				_ = os.Remove(path("webui", "secrets", item.ID+".txt"))
			}
		}
		if g.Kind == "subscription" {
			_ = os.Remove(path("webui", "secrets", "subscription-"+g.ID+".txt"))
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "removed": len(removed)})
	case "/api/groups/refresh":
		if g.Kind != "subscription" {
			errorOut(w, 409, "Для старой группы источник недоступен. Импортируйте ссылку заново.")
			return
		}
		if active() {
			errorOut(w, 409, "Отключите VPN перед обновлением подписки")
			return
		}
		source, e := os.ReadFile(path("webui", "secrets", "subscription-"+req.ID+".txt"))
		if e != nil {
			errorOut(w, 409, "Источник подписки не найден")
			return
		}
		links, _, _, nextMeta, e := subscriptionFetcher(strings.TrimSpace(string(source)))
		if e != nil {
			errorOut(w, 400, safeError(e))
			return
		}
		oldIDs := map[string]Profile{}
		oldNames := map[string][]Profile{}
		for _, item := range p.Items {
			if item.GroupID == g.ID {
				oldIDs[fingerprintForProfile(item)] = item
				if key := profileLabel(item); key != "" {
					oldNames[key] = append(oldNames[key], item)
				}
			}
		}
		nameCounts := map[string]int{}
		for _, link := range links {
			if key := subscriptionLabel(link); key != "" {
				nameCounts[key]++
			}
		}
		usedOld := map[string]bool{}
		updated := []Profile{}
		created := []string{}
		// Old secrets are kept for recovery if writing a refreshed profile
		// or the selected Xray configuration fails.
		replaced := map[string][]byte{}
		committed := false
		defer func() {
			if !committed {
				for id, oldSecret := range replaced {
					_ = atomic(path("webui", "secrets", id+".txt"), oldSecret, 0600)
				}
				for _, id := range created {
					if safeID.MatchString(id) {
						_ = os.Remove(path("webui", "secrets", id+".txt"))
					}
				}
			}
		}()
		valid := 0
		issues := []ImportIssue{}
		seen := map[string]int{}
		for index, line := range links {
			fp := secretFingerprint(line)
			if previous, found := seen[fp]; found {
				issues = append(issues, newImportIssue(index, line, duplicateInSourceReason(previous, links[previous])))
				continue
			}
			seen[fp] = index
			out, ip, name, err := profileOutbound(line)
			if err != nil {
				issues = append(issues, newImportIssue(index, line, importFailureReason(err)))
				continue
			}
			cfg, err := configFor(out)
			if err == nil {
				err = testConfig(cfg)
			}
			if err != nil {
				issues = append(issues, newImportIssue(index, line, importFailureReason(err)))
				continue
			}
			_, tr, sec, err := metaFromOutbound(out)
			if err != nil {
				issues = append(issues, newImportIssue(index, line, importFailureReason(err)))
				continue
			}
			item, found := oldIDs[fp]
			if found && usedOld[item.ID] {
				found = false
			}
			// If the upstream auto profile changes its IP but retains its
			// unique name, keep its ID and user's active selection.
			if !found {
				key := subscriptionLabel(line)
				if key != "" && nameCounts[key] == 1 && len(oldNames[key]) == 1 && !usedOld[oldNames[key][0].ID] {
					item, found = oldNames[key][0], true
				}
			}
			if !found {
				item = Profile{ID: randomID(), Source: "subscription", GroupID: g.ID, Fingerprint: fp}
				if err = atomic(path("webui", "secrets", item.ID+".txt"), []byte(line), 0600); err != nil {
					errorOut(w, 500, "Не удалось сохранить обновление")
					return
				}
				created = append(created, item.ID)
			} else {
				secretPath := path("webui", "secrets", item.ID+".txt")
				oldSecret, readErr := os.ReadFile(secretPath)
				if readErr != nil {
					errorOut(w, 500, "Сохранённый профиль недоступен")
					return
				}
				if string(oldSecret) != line {
					if _, alreadySaved := replaced[item.ID]; !alreadySaved {
						replaced[item.ID] = oldSecret
					}
					if err = atomic(secretPath, []byte(line), 0600); err != nil {
						errorOut(w, 500, "Не удалось обновить профиль")
						return
					}
				}
			}
			usedOld[item.ID] = true
			item.Name = safeLabel(name, 80)
			item.Address = ip
			item.Transport = tr
			item.Security = sec
			item.Fingerprint = fp
			updated = append(updated, item)
			valid++
		}
		if valid == 0 {
			jsonResponse(w, 422, map[string]any{"ok": false, "error": "Ни один сервер не прошёл проверку. Старые профили сохранены.", "issues": issues})
			return
		}
		oldActive := p.Active
		oldGroup := []Profile{}
		kept := []Profile{}
		for _, item := range p.Items {
			if item.GroupID == g.ID {
				oldGroup = append(oldGroup, item)
			} else {
				kept = append(kept, item)
			}
		}
		// Do not delete ANY formerly working server if even one source entry
		// was rejected. In a partial refresh we cannot reliably correlate
		// a changed link with the old profile fingerprint.
		retained := 0
		if len(issues) != 0 {
			for _, old := range oldGroup {
				if !containsID(updated, old.ID) {
					updated = append(updated, old)
					retained++
				}
			}
		}
		p.Items = append(kept, updated...)
		// If the selected server disappeared, choose a still-valid one and safely update the config.
		if containsActive(oldGroup, oldActive) && !containsActive(updated, oldActive) {
			// Selected ID must remain valid until new config is applied.
			if err := selectProfileLocked(&p, updated[0].ID); err != nil {
				errorOut(w, 409, "Не удалось выбрать обновлённый сервер")
				return
			}
		}
		for i := range p.Groups {
			if p.Groups[i].ID == g.ID {
				p.Groups[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				p.Groups[i].Meta = nextMeta
			}
		}
		// Selected automatic profile may now have a different endpoint,
		// while retaining the same ID. Update client.json and server-ip.txt
		// too, otherwise the next Start would use its previous server.
		var originalConfig, originalServerIP []byte
		configChanged := false
		if _, needsUpdate := replaced[p.Active]; needsUpdate {
			originalConfig, _ = os.ReadFile(path("config", "client.json"))
			originalServerIP, _ = os.ReadFile(path("config", "server-ip.txt"))
			secret, readErr := os.ReadFile(path("webui", "secrets", p.Active+".txt"))
			if readErr != nil {
				errorOut(w, 500, "Не удалось прочитать выбранный профиль")
				return
			}
			out, ip, _, parseErr := profileOutbound(string(secret))
			if parseErr != nil {
				errorOut(w, 500, "Выбранный профиль некорректен")
				return
			}
			cfg, cfgErr := configFor(out)
			if cfgErr == nil {
				cfgErr = testConfig(cfg)
			}
			if cfgErr != nil {
				errorOut(w, 500, "Xray отклонил выбранный профиль")
				return
			}
			if cfgErr = applyConfig(cfg, []byte(ip+"\n")); cfgErr != nil {
				errorOut(w, 500, "Не удалось обновить выбранный профиль")
				return
			}
			configChanged = true
		}
		if e = writeProfiles(p); e != nil {
			if configChanged && len(originalConfig) > 0 && len(originalServerIP) > 0 {
				_ = applyConfig(originalConfig, originalServerIP)
			}
			errorOut(w, 500, "Ошибка сохранения подписки")
			return
		}
		committed = true
		removed := 0
		removedNames := []string{}
		for _, item := range oldGroup {
			if !containsID(updated, item.ID) && safeID.MatchString(item.ID) {
				_ = os.Remove(path("webui", "secrets", item.ID+".txt"))
				removed++
				removedNames = append(removedNames, safeLabel(item.Name, 60))
			}
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "received": len(links), "total": len(updated), "valid": valid, "added": len(created), "removed": removed, "removedNames": removedNames, "retained": retained, "issues": issues, "partial": len(issues) != 0})
	default:
		errorOut(w, 404, "Неизвестная операция")
	}
}
func containsActive(items []Profile, id string) bool { return containsID(items, id) }
func containsID(items []Profile, id string) bool {
	for _, v := range items {
		if v.ID == id {
			return true
		}
	}
	return false
}

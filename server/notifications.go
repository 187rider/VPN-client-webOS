package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Event notifications never start, stop, or repair the VPN. Network recovery
// remains under the independently guarded CVPN Engine.
const notificationInterval = 5 * time.Second

type vpnNoticeState struct {
	kind    string // connected, direct, degraded, transitional
	profile string
}

type observedIP struct {
	IP      string
	Country string
}

var notifySystemToast = systemStatusToast
var lookupObservedIP = observePublicIPv4

func noticeState() vpnNoticeState {
	if engineIsTransitioning() {
		return vpnNoticeState{kind: "transitional"}
	}
	s := status()
	if s["needsRecovery"] == true {
		return vpnNoticeState{kind: "degraded"}
	}
	if s["connected"] == true {
		if s["phase"] != "ready" {
			return vpnNoticeState{kind: "transitional"}
		}
		p, _ := s["profile"].(string)
		return vpnNoticeState{kind: "connected", profile: p}
	}
	if !directNetworkRestored() {
		return vpnNoticeState{kind: "degraded"}
	}
	return vpnNoticeState{kind: "direct"}
}

// A disconnected status must not be misrepresented as restored internet when
// route/DNS remnants are still installed. This is read-only and conservative.
func directNetworkRestored() bool {
	if active() {
		return false
	}
	if _, err := os.Stat("/sys/class/net/cvpn0"); err == nil {
		return false
	}
	routes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return false
	}
	haveDefault := false
	for _, line := range strings.Split(string(routes), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		if f[0] == "cvpn0" {
			return false
		}
		if f[1] == "00000000" && f[7] == "00000000" {
			haveDefault = true
		}
	}
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(mounts), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[1] == "/var/lib/misc/resolv.conf" {
			return false
		}
	}
	return haveDefault
}

// Country flags are optional display hints, never routing or GeoIP evidence.
// Explicit provider emoji has priority; otherwise match names or leading codes.
var noticeFlagRE = regexp.MustCompile(`[\x{1F1E6}-\x{1F1FF}]{2}`)
var noticeCodeRE = regexp.MustCompile(`^\s*(?:\[([A-Z]{2})\]|\(([A-Z]{2})\)|([A-Z]{2})[\s|:·-])`)
var noticeCountryAliases = []struct {
	code, search string
}{
	{"NL", "нидерланд|голланди|netherlands|holland|амстердам|amsterdam|роттердам|rotterdam"},
	{"DE", "германи|germany|deutschland|берлин|berlin|франкфурт|frankfurt|мюнхен|munich"},
	{"US", "сша|соединенн|соединённ|united states|america|америк|санта-клара|santa clara|нью-йорк|new york|сан-франциско|san francisco|лос-анджелес|los angeles"},
	{"RU", "росси|russia|москва|moscow|петербург|petersburg"},
	{"FR", "франци|france|париж|paris|марсел|marseille"},
	{"TR", "турци|turkey|türkiye|turkiye|стамбул|istanbul|анкара|ankara"},
	{"GB", "британи|англи|united kingdom|great britain|лондон|london"},
	{"SE", "швец|sweden|стокгольм|stockholm"},
	{"FI", "финлянди|finland|хельсинк|helsinki"},
	{"NO", "норвеги|norway|осло|oslo"},
	{"DK", "дании|дания|denmark|копенгаген|copenhagen"},
	{"PL", "польш|poland|варшав|warsaw"},
	{"CZ", "чехи|czech|праг|prague"},
	{"AT", "австри|austria|вен[а-я]|vienna"},
	{"CH", "швейцари|switzerland|zurich|цюрих"},
	{"IT", "итали|italy|рим|rome|милан|milan"},
	{"ES", "испани|spain|мадрид|madrid|барселон|barcelona"},
	{"PT", "португали|portugal|лиссабон|lisbon"},
	{"BE", "бельги|belgium|брюссел|brussels"},
	{"IE", "ирланд|ireland|дублин|dublin"},
	{"UA", "украин|ukraine|киев|kyiv"},
	{"KZ", "казахстан|kazakhstan|алмат|almaty"},
	{"BY", "беларус|belarus|минск|minsk"},
	{"JP", "япони|japan|токио|tokyo|осак|osaka"},
	{"KR", "коре|south korea|сеул|seoul"},
	{"CN", "кита|china|пекин|beijing|шанхай|shanghai"},
	{"SG", "сингапур|singapore"},
	{"HK", "гонконг|hong kong"},
	{"TW", "тайван|taiwan|тайбэй|taipei"},
	{"IN", "инди|india|мумбаи|mumbai|дели|delhi"},
	{"AE", "оаэ|emirates|дубай|dubai"},
	{"IL", "израил|israel|тель-авив|tel aviv"},
	{"CA", "канад|canada|торонто|toronto|ванкувер|vancouver"},
	{"BR", "бразили|brazil|сан-паулу|sao paulo"},
	{"AU", "австрали|australia|сидне|sydney"},
	{"NZ", "зеланди|new zealand|окленд|auckland"},
	{"ID", "индонези|indonesia|джакарт|jakarta"},
	{"TH", "таиланд|тайланд|thailand|бангкок|bangkok"},
	{"VN", "вьетнам|vietnam|ханой|hanoi|хошимин|ho chi minh"},
	{"RO", "румыни|romania|бухарест|bucharest"},
	{"HU", "венгри|hungary|будапешт|budapest"},
	{"GR", "греци|greece|афин|athens"},
	{"EU", "автовыбор|автоматический выбор|automatic selection|european union"},
}

func countryFlagForNotice(name string) string {
	if existing := noticeFlagRE.FindString(name); existing != "" {
		return existing
	}
	// Country code must be a discrete leading token, not a random substring.
	matches := noticeCodeRE.FindStringSubmatch(name)
	if len(matches) > 0 {
		for _, value := range matches[1:] {
			if code := strings.ToUpper(value); len(code) == 2 && validNoticeCode(code) {
				return codeFlag(code)
			}
		}
	}
	normalized := strings.ToLower(name)
	for _, entry := range noticeCountryAliases {
		for _, key := range strings.Split(entry.search, "|") {
			if strings.Contains(normalized, key) {
				return codeFlag(entry.code)
			}
		}
	}
	return ""
}

// A narrow allow-list of the ISO codes used in typical VPN subscriptions.
// The original emoji, when present, works for all countries without a list.
func validNoticeCode(code string) bool {
	switch code {
	case "NL", "DE", "US", "RU", "FR", "TR", "GB", "SE", "FI", "NO", "DK", "PL", "CZ", "AT", "CH", "IT", "ES", "PT", "BE", "IE", "UA", "KZ", "BY", "JP", "KR", "CN", "SG", "HK", "TW", "IN", "AE", "IL", "CA", "BR", "AU", "NZ", "ID", "TH", "VN", "RO", "HU", "GR", "EU", "LV", "LT", "EE", "RS", "HR", "SI", "SK", "MD", "GE", "AM", "AZ", "UZ", "KG", "TJ", "TM", "MY", "PH", "KH", "LA", "MM", "BD", "PK", "MX", "AR", "CL", "CO", "PE", "ZA", "EG", "MA", "NG", "KE", "SA", "QA", "BH", "OM", "KW", "CY", "LU", "IS", "MT":
		return true
	}
	return false
}
func codeFlag(code string) string {
	if len(code) != 2 {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(code[0]-'A'), 0x1F1E6 + rune(code[1]-'A')})
}
func noticeProfileLabel(name string) string {
	name = cleanNoticeText(name, 32)
	flag := countryFlagForNotice(name)
	if flag != "" {
		name = strings.ReplaceAll(name, flag, "")
	}
	name = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(name), "·|:-— "))
	if name == "" {
		name = "VPN-сервер"
	}
	if flag != "" {
		return flag + " " + name
	}
	return name
}
func noticeProfileName(id string) string {
	p, err := readProfiles()
	if err == nil {
		for _, item := range p.Items {
			if item.ID == id {
				return noticeProfileLabel(item.Name)
			}
		}
	}
	return "VPN-сервер"
}

var tvNoticeLanguageCache struct {
	sync.Mutex
	lang  string
	until time.Time
}

func tvNoticeLanguage() string {
	pref := readLanguageSetting()
	if pref == "en" || pref == "ru" {
		return pref
	}
	tvNoticeLanguageCache.Lock()
	defer tvNoticeLanguageCache.Unlock()
	if time.Now().Before(tvNoticeLanguageCache.until) {
		return tvNoticeLanguageCache.lang
	}
	tvNoticeLanguageCache.lang = languageForTV("ru")
	tvNoticeLanguageCache.until = time.Now().Add(10 * time.Minute)
	return tvNoticeLanguageCache.lang
}
func tvEN() bool { return tvNoticeLanguage() == "en" }

func cleanNoticeText(s string, limit int) string {
	out := make([]rune, 0, limit)
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "VPN-сервер"
	}
	return string(out)
}

func countryRU(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "RU":
		return "Россия"
	case "NL":
		return "Нидерланды"
	case "DE":
		return "Германия"
	case "US":
		return "США"
	case "SE":
		return "Швеция"
	case "CN":
		return "Китай"
	case "JP":
		return "Япония"
	case "FR":
		return "Франция"
	case "GB":
		return "Великобритания"
	default:
		return ""
	}
}

// Optional, privacy-sensitive check: a single external HTTPS request only
// after a stable state transition, not on a periodic polling cycle. IPv4 is
// forced so the probe follows the same IPv4 routes as CVPN's TUN.
func observePublicIPv4(ctx context.Context) (observedIP, error) {
	if os.Getenv("CVPN_IP_LOOKUP") == "0" {
		return observedIP{}, errors.New("IP lookup disabled")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(c context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(c, "tcp4", addr)
		},
		TLSHandshakeTimeout: 3 * time.Second,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipwho.is/", nil)
	if err != nil {
		return observedIP{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return observedIP{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return observedIP{}, fmt.Errorf("IP endpoint HTTP %d", resp.StatusCode)
	}
	var v struct {
		Success     bool   `json:"success"`
		IP          string `json:"ip"`
		CountryCode string `json:"country_code"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&v); err != nil {
		return observedIP{}, err
	}
	ip := net.ParseIP(v.IP)
	if !v.Success || ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() {
		return observedIP{}, errors.New("public IPv4 not verified")
	}
	return observedIP{IP: ip.String(), Country: countryRU(v.CountryCode)}, nil
}

func buildStateToast(st vpnNoticeState, observed observedIP) string {
	en := tvEN()
	var msg string
	switch st.kind {
	case "connected":
		label := noticeProfileName(st.profile)
		if en {
			msg = "CVPN · " + label + " · Connected"
		} else {
			msg = "CVPN · " + label + " · VPN подключён"
		}
	case "direct":
		if en {
			msg = "CVPN · VPN disconnected"
		} else {
			msg = "CVPN · VPN отключён"
		}
		// Observer may have a verified public IP from another path. This is not
		// looked up solely for a toast; preserve it for an explicit caller.
		if observed.Country != "" {
			msg += " · " + cleanNoticeText(observed.Country, 16)
		}
		if observed.IP != "" {
			msg += " · IP " + cleanNoticeText(observed.IP, 45)
		}
	case "degraded":
		if en {
			msg = "CVPN · Network needs attention"
		} else {
			msg = "CVPN · Сбой сети. Проверьте подключение"
		}
	default:
		return ""
	}
	return limitToast(msg)
}
func limitToast(s string) string {
	r := []rune(s)
	if len(r) <= 60 {
		return s
	}
	// Never split a regional-indicator pair (country emoji) in half.
	n := 59
	if n < len(r) && r[n-1] >= 0x1F1E6 && r[n-1] <= 0x1F1FF && r[n] >= 0x1F1E6 && r[n] <= 0x1F1FF {
		n--
	}
	return string(r[:n]) + "…"
}

// webOS notification icon must be a local 80x80 PNG. Reuse the existing
// installed CVPN launcher icon: no write access to webOS system partitions.
var toastIconPath = "/media/developer/apps/usr/palm/applications/su.cvpn.webos/icon.png"

func toastPayload(message string) ([]byte, bool) {
	fields := map[string]any{
		"sourceId": "su.cvpn.webos", "message": message,
		"noaction": true, "persistent": false,
	}
	icon := false
	if info, err := os.Stat(toastIconPath); err == nil && info.Mode().IsRegular() {
		fields["iconUrl"] = "file://" + toastIconPath
		icon = true
	}
	data, _ := json.Marshal(fields)
	return data, icon
}

func sendLunaToast(ctx context.Context, binary string, data []byte) error {
	output, err := exec.CommandContext(ctx, binary, "-n", "1", "-f", "-a",
		"com.webos.service.secondscreen.gateway", "luna://com.webos.notification/createToast", string(data)).CombinedOutput()
	if err != nil {
		output2, err2 := exec.CommandContext(ctx, binary, "-n", "1", "-f",
			"luna://com.webos.notification/createToast", string(data)).CombinedOutput()
		if err2 == nil {
			output = output2
			err = nil
		} else {
			return fmt.Errorf("luna-send Toast: %w (fallback: %v, out: %s)", err, err2, string(output2))
		}
	}
	var response struct {
		ReturnValue bool   `json:"returnValue"`
		ErrorText   string `json:"errorText"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return fmt.Errorf("invalid Toast response: %w", err)
	}
	if !response.ReturnValue {
		return fmt.Errorf("Toast rejected: %s", response.ErrorText)
	}
	return nil
}

func systemStatusToast(message string) error {
	if message == "" {
		return nil
	}
	data, iconIncluded := toastPayload(message)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	binary, err := lunaSendExecutable()
	if err != nil {
		return err
	}
	err = sendLunaToast(ctx, binary, data)
	if err == nil || !iconIncluded {
		return err
	}
	// A few older TV firmwares reject iconUrl. Retry without the icon,
	// so notifications continue working rather than failing completely.
	log.Printf("toast icon rejected; retrying without icon: %v", err)
	plain, _ := json.Marshal(map[string]any{"sourceId": "su.cvpn.webos", "message": message,
		"noaction": true, "persistent": false})
	return sendLunaToast(ctx, binary, plain)
}

// Foreground API requests announce their result immediately. The observer
// must not repeat those notifications 10 seconds later. It can still catch
// watchdog-driven state changes or unexpected failures.
var manualNotice struct {
	sync.Mutex
	state vpnNoticeState
	time  time.Time
}

func markManualNotice(st vpnNoticeState) {
	manualNotice.Lock()
	manualNotice.state, manualNotice.time = st, time.Now()
	manualNotice.Unlock()
}
func recentlyManuallyAnnounced(st vpnNoticeState) bool {
	manualNotice.Lock()
	defer manualNotice.Unlock()
	return st == manualNotice.state && time.Since(manualNotice.time) < 60*time.Second
}
func sendStatusMessage(msg string) bool {
	if err := notifySystemToast(limitToast(msg)); err != nil {
		log.Printf("status notification unavailable: %v", err)
		return false
	}
	return true
}
func announceVPNConnecting(id string, switching bool) {
	name := noticeProfileName(id)
	if tvEN() {
		if switching {
			sendStatusMessage("CVPN · Switching to " + name + "…")
		} else {
			sendStatusMessage("CVPN · Connecting to " + name + "…")
		}
		return
	}
	if switching {
		sendStatusMessage("CVPN · Переключение: " + name + "…")
	} else {
		sendStatusMessage("CVPN · Подключение: " + name + "…")
	}
}
func announceVPNDisconnecting() {
	if tvEN() {
		sendStatusMessage("CVPN · Disconnecting VPN…")
	} else {
		sendStatusMessage("CVPN · Отключение VPN…")
	}
}
func announceVPNConnected(id string) {
	st := vpnNoticeState{kind: "connected", profile: id}
	if sendStatusMessage(buildStateToast(st, observedIP{})) {
		markManualNotice(st)
	}
}
func announceVPNDisconnected() {
	st := vpnNoticeState{kind: "direct"}
	if sendStatusMessage(buildStateToast(st, observedIP{})) {
		markManualNotice(st)
	}
}
func announceVPNActionError(action string) {
	if tvEN() {
		if action == "stop" {
			sendStatusMessage("CVPN · Could not disconnect. Check network")
		} else {
			sendStatusMessage("CVPN · Connection failed. Check network")
		}
		return
	}
	if action == "stop" {
		sendStatusMessage("CVPN · Не удалось отключить VPN")
	} else {
		sendStatusMessage("CVPN · Подключение не удалось")
	}
}

// transitionTracker is pure state; see tests for duplicate suppression and
// intermediate switching behavior.
type transitionTracker struct {
	last        vpnNoticeState
	candidate   vpnNoticeState
	stableCount int
	initialized bool
}

func (t *transitionTracker) next(current vpnNoticeState) (vpnNoticeState, bool) {
	if current.kind == "transitional" {
		t.candidate = vpnNoticeState{}
		t.stableCount = 0
		return vpnNoticeState{}, false
	}
	if !t.initialized {
		t.initialized = true
		t.last = current
		return vpnNoticeState{}, false
	}
	if current == t.last {
		t.candidate = vpnNoticeState{}
		t.stableCount = 0
		return vpnNoticeState{}, false
	}
	if current != t.candidate {
		t.candidate = current
		t.stableCount = 1
		return vpnNoticeState{}, false
	}
	t.stableCount++
	if t.stableCount < 2 {
		return vpnNoticeState{}, false
	}
	t.last = current
	t.candidate = vpnNoticeState{}
	t.stableCount = 0
	return current, true
}

// A single read-only observer: suppress startup announcements, wait for two
// stable samples, and do not send intermediate messages while an engine action
// holds mx. A profile switch produces one final connected notification.
func watchVPNEvents(ctx context.Context) {
	ticker := time.NewTicker(notificationInterval)
	defer ticker.Stop()
	// Capture startup state immediately to avoid missing quick transitions.
	initial := noticeState()
	tracker := transitionTracker{last: initial, initialized: initial.kind != "transitional"}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !mx.TryLock() {
			continue
		}
		current := noticeState()
		mx.Unlock()
		confirmed, changed := tracker.next(current)
		if !changed {
			continue
		}
		current = confirmed
		// Record confirmed transitions even when they originate in the independent
		// watchdog (not through a mobile API request). These are observations only.
		switch current.kind {
		case "connected":
			recordEvent("observed_connected", "Наблюдатель подтвердил активный VPN-маршрут")
		case "direct":
			recordEvent("observed_direct", "Наблюдатель подтвердил обычный сетевой маршрут")
		case "degraded":
			recordEvent("observed_degraded", "Обнаружено некорректное состояние VPN; проверьте восстановление сети")
		}
		// No external HTTPS/public-IP call on the notification path.
		// Fast foreground actions already sent the corresponding TV toast.
		if recentlyManuallyAnnounced(current) {
			continue
		}
		sendStatusMessage(buildStateToast(current, observedIP{}))
	}
}

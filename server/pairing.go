package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const pairDuration = 3 * time.Minute
const browserDuration = 30 * 24 * time.Hour
const pairingCookie = "cvpn_sid"

type pairingRequest struct {
	ID       string
	Secret   string
	Name     string
	Created  time.Time
	Approved bool
}
type tvPairState struct {
	CodeHash [32]byte
	Code     string
	Expires  time.Time
	Failed   int
	Requests map[string]*pairingRequest
}
type browserSession struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"`
	CSRF    string    `json:"csrf"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}
type browserSessions struct {
	Items []browserSession `json:"items"`
}

var pairLock sync.Mutex
var pairState tvPairState
var browserLock sync.Mutex

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func constantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func localTVRequest(r *http.Request) bool {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func loadBrowserSessions() browserSessions {
	var d browserSessions
	b, e := os.ReadFile(path("webui", "sessions.json"))
	if e == nil {
		if json.Unmarshal(b, &d) != nil {
			d.Items = nil
		}
	}
	return d
}
func saveBrowserSessions(d browserSessions) error {
	return atomic(path("webui", "sessions.json"), mustJSON(d), 0600)
}
func readBrowserSession(r *http.Request) (browserSession, bool) {
	c, e := r.Cookie(pairingCookie)
	if e != nil || len(c.Value) != 64 {
		return browserSession{}, false
	}
	digest := sha256.Sum256([]byte(c.Value))
	wanted := hex.EncodeToString(digest[:])
	browserLock.Lock()
	defer browserLock.Unlock()
	for _, v := range loadBrowserSessions().Items {
		if time.Now().Before(v.Expires) && constantEqual(v.Hash, wanted) {
			return v, true
		}
	}
	return browserSession{}, false
}
func cookieOn(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: pairingCookie, Value: token, Path: "/", MaxAge: int(browserDuration.Seconds()), Expires: time.Now().Add(browserDuration), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}
func cookieOff(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: pairingCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}
func createBrowserSession(w http.ResponseWriter, r *http.Request, name string) error {
	token, e := randomSecret(32)
	if e != nil {
		return e
	}
	csrf, e := randomSecret(24)
	if e != nil {
		return e
	}
	h := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	s := browserSession{ID: randomID(), Hash: hex.EncodeToString(h[:]), CSRF: csrf, Name: safeLabel(name, 60), Created: now, Expires: now.Add(browserDuration)}
	browserLock.Lock()
	defer browserLock.Unlock()
	data := loadBrowserSessions()
	keep := []browserSession{}
	for _, v := range data.Items {
		if time.Now().Before(v.Expires) {
			keep = append(keep, v)
		}
	}
	if len(keep) >= 24 {
		keep = keep[len(keep)-23:]
	}
	data.Items = append(keep, s)
	if e := saveBrowserSessions(data); e != nil {
		return e
	}
	cookieOn(w, r, token)
	return nil
}
func tvLANAddress() string {
	for _, name := range []string{"eth0", "wlan0", "wlan1", "en0", "en1"} {
		nic, e := net.InterfaceByName(name)
		if e != nil {
			continue
		}
		addrs, e := nic.Addrs()
		if e != nil {
			continue
		}
		for _, a := range addrs {
			host, _, e := net.ParseCIDR(a.String())
			if e == nil && host.To4() != nil && host.IsPrivate() {
				return host.String()
			}
		}
	}
	return ""
}
func issueTVPin() (string, error) {
	codeBytes := make([]byte, 4)
	if _, e := rand.Read(codeBytes); e != nil {
		return "", e
	}
	n := uint32(codeBytes[0])<<24 | uint32(codeBytes[1])<<16 | uint32(codeBytes[2])<<8 | uint32(codeBytes[3])
	code := fmt.Sprintf("%06d", n%1000000)
	pairLock.Lock()
	pairState = tvPairState{CodeHash: sha256.Sum256([]byte(code)), Code: code, Expires: time.Now().Add(pairDuration), Requests: map[string]*pairingRequest{}}
	pairLock.Unlock()
	return code, nil
}

func (a *app) tvPairingPage(w http.ResponseWriter, r *http.Request) {
	if !localTVRequest(r) {
		http.Error(w, "Only accessible on the television", 403)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "GET required", 405)
		return
	}
	code, err := issueTVPin()
	if err != nil {
		http.Error(w, "Cannot create code", 500)
		return
	}
	b, e := frontend.ReadFile("public/tv-screen.html")
	if e != nil {
		http.Error(w, "TV page unavailable", 500)
		return
	}
	ip := tvLANAddress()
	addr := "Определите IP в настройках сети LG"
	if ip != "" {
		addr = "http://" + ip + ":8765"
	}
	content := strings.NewReplacer("__PIN__", html.EscapeString(code[:3]+" "+code[3:]), "__ADDRESS__", html.EscapeString(addr)).Replace(string(b))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(content))
}
func (a *app) handlePairingAPI(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/auth":
		if r.Method != "GET" {
			errorOut(w, 405, "GET required")
			return true
		}
		if localTVRequest(r) && clientAuthorized(r, a.key) {
			jsonResponse(w, 200, map[string]any{"ok": true, "legacy": true})
			return true
		}
		s, ok := readBrowserSession(r)
		if !ok {
			errorOut(w, 401, "device not paired")
			return true
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "csrf": s.CSRF, "deviceId": s.ID, "deviceName": s.Name})
		return true
	case "/api/pairing/request":
		if !requirePost(w, r) {
			return true
		}
		if exceededFailures(r.RemoteAddr) {
			errorOut(w, 429, "Слишком много попыток. Запустите сопряжение позже.")
			return true
		}
		var req struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if decodeJSON(r, &req) != nil {
			errorOut(w, 400, "Некорректные данные")
			return true
		}
		req.Code = strings.ReplaceAll(strings.TrimSpace(req.Code), " ", "")
		pairLock.Lock()
		if time.Now().After(pairState.Expires) || pairState.Expires.IsZero() || pairState.Failed >= 6 {
			pairLock.Unlock()
			errorOut(w, 429, "На телевизоре откройте новое сопряжение")
			return true
		}
		digest := sha256.Sum256([]byte(req.Code))
		if len(req.Code) != 6 || subtle.ConstantTimeCompare(digest[:], pairState.CodeHash[:]) != 1 {
			pairState.Failed++
			pairLock.Unlock()
			recordFailure(r.RemoteAddr)
			errorOut(w, 401, "Неверный код с телевизора")
			return true
		}
		label := safeLabel(req.Name, 50)
		if label == "" {
			label = "Телефон или компьютер"
		}
		// The TV-displayed PIN is the proof of possession. Create the session
		// while holding pairLock so that concurrent requests cannot reuse it.
		if e := createBrowserSession(w, r, label); e != nil {
			pairLock.Unlock()
			errorOut(w, 500, "Не удалось сохранить устройство")
			return true
		}
		pairState = tvPairState{}
		pairLock.Unlock()
		clearFailures(r.RemoteAddr)
		jsonResponse(w, 200, map[string]any{"ok": true, "state": "approved"})
		return true
	case "/api/pairing/status":
		if r.Method != "GET" {
			errorOut(w, 405, "GET required")
			return true
		}
		id := r.URL.Query().Get("id")
		token := r.URL.Query().Get("token")
		pairLock.Lock()
		pending, exists := pairState.Requests[id]
		if !exists || time.Now().After(pairState.Expires) || !constantEqual(pending.Secret, token) {
			pairLock.Unlock()
			errorOut(w, 410, "Запрос сопряжения истёк")
			return true
		}
		if !pending.Approved {
			pairLock.Unlock()
			jsonResponse(w, 200, map[string]any{"state": "pending"})
			return true
		}
		delete(pairState.Requests, id)
		label := pending.Name
		pairLock.Unlock()
		if e := createBrowserSession(w, r, label); e != nil {
			errorOut(w, 500, "Не удалось сохранить устройство")
			return true
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "state": "approved"})
		return true
	case "/api/pairing/tv-pending":
		if !localTVRequest(r) {
			errorOut(w, 403, "TV only")
			return true
		}
		if r.Method != "GET" {
			errorOut(w, 405, "GET required")
			return true
		}
		pairLock.Lock()
		list := []map[string]string{}
		if time.Now().Before(pairState.Expires) {
			for _, p := range pairState.Requests {
				if !p.Approved {
					list = append(list, map[string]string{"id": p.ID, "name": p.Name})
				}
			}
		}
		left := int(time.Until(pairState.Expires).Seconds())
		if left < 0 {
			left = 0
		}
		pairLock.Unlock()
		jsonResponse(w, 200, map[string]any{"requests": list, "remaining": left})
		return true
	case "/api/pairing/tv-approve", "/api/pairing/tv-deny":
		if !localTVRequest(r) {
			errorOut(w, 403, "TV only")
			return true
		}
		if !requirePost(w, r) {
			return true
		}
		var req struct {
			ID string `json:"id"`
		}
		if decodeJSON(r, &req) != nil {
			errorOut(w, 400, "invalid request")
			return true
		}
		pairLock.Lock()
		p, ok := pairState.Requests[req.ID]
		if !ok || time.Now().After(pairState.Expires) {
			pairLock.Unlock()
			errorOut(w, 410, "Request expired")
			return true
		}
		if r.URL.Path == "/api/pairing/tv-deny" {
			delete(pairState.Requests, req.ID)
		} else {
			p.Approved = true
		}
		pairLock.Unlock()
		jsonResponse(w, 200, map[string]any{"ok": true})
		return true
	}
	return false
}
func (a *app) handleDeviceAPI(w http.ResponseWriter, r *http.Request, legacy bool) bool {
	switch r.URL.Path {
	case "/api/devices":
		if r.Method != "GET" {
			errorOut(w, 405, "GET required")
			return true
		}
		caller, found := readBrowserSession(r)
		browserLock.Lock()
		data := loadBrowserSessions()
		browserLock.Unlock()
		items := []map[string]any{}
		for _, v := range data.Items {
			if time.Now().After(v.Expires) {
				continue
			}
			items = append(items, map[string]any{"id": v.ID, "name": v.Name, "created": v.Created, "expires": v.Expires, "current": found && v.ID == caller.ID})
		}
		jsonResponse(w, 200, map[string]any{"items": items})
		return true
	case "/api/devices/revoke":
		if !requirePost(w, r) {
			return true
		}
		var req struct {
			ID string `json:"id"`
		}
		if decodeJSON(r, &req) != nil {
			errorOut(w, 400, "invalid request")
			return true
		}
		browserLock.Lock()
		data := loadBrowserSessions()
		items := []browserSession{}
		for _, v := range data.Items {
			if v.ID != req.ID {
				items = append(items, v)
			}
		}
		data.Items = items
		e := saveBrowserSessions(data)
		browserLock.Unlock()
		if e != nil {
			errorOut(w, 500, "storage unavailable")
			return true
		}
		jsonResponse(w, 200, map[string]any{"ok": true})
		return true
	case "/api/auth/logout":
		if !requirePost(w, r) {
			return true
		}
		if !legacy {
			sess, found := readBrowserSession(r)
			if found {
				browserLock.Lock()
				data := loadBrowserSessions()
				keep := []browserSession{}
				for _, v := range data.Items {
					if v.ID != sess.ID {
						keep = append(keep, v)
					}
				}
				data.Items = keep
				_ = saveBrowserSessions(data)
				browserLock.Unlock()
			}
		}
		cookieOff(w)
		jsonResponse(w, 200, map[string]any{"ok": true})
		return true
	}
	return false
}

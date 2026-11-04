package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func runEngine(action string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", path("bin", "cvpn.sh"), action)
	b, e := cmd.CombinedOutput()
	msg := string(b)
	if len(msg) > 3000 {
		msg = msg[len(msg)-3000:]
	}
	if ctx.Err() != nil {
		return "", errors.New("VPN-движок не ответил вовремя; проверьте состояние сети")
	}
	return msg, e
}
func (a *app) connectProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if e := decodeJSON(r, &req); e != nil {
		errorOut(w, 400, "Некорректный запрос")
		return
	}
	mx.Lock()
	defer mx.Unlock()
	p, e := readProfiles()
	if e != nil {
		errorOut(w, 500, "Не удалось загрузить профили")
		return
	}
	if !containsID(p.Items, req.ID) {
		errorOut(w, 404, "Сервер не найден")
		return
	}
	if active() && status()["needsRecovery"] == true {
		recordEvent("auto_recovery", "Обнаружено повреждённое состояние; выполняется автоматическая очистка")
		output, e := runEngine("stop")
		if e != nil || active() || !strings.Contains(output, "NETWORK_RESTORED=YES") {
			recordEvent("recovery_failed", "Не удалось автоматически очистить повреждённое состояние сети")
			errorOut(w, 409, "Не удалось безопасно очистить повреждённое состояние сети. Попробуйте восстановить сеть вручную.")
			return
		}
	}
	if active() && p.Active == req.ID {
		jsonResponse(w, 200, map[string]any{"ok": true, "unchanged": true, "status": status()})
		return
	}
	previous := p.Active
	switching := active() && p.Active != req.ID
	if switching {
		// Crucial: reject an invalid target BEFORE disconnecting the working VPN.
		// Preflight may stage a temporary Xray test JSON, but never changes
		// the selected profile, DNS, routes, TUN or running Xray process.
		if state := status(); state["needsRecovery"] == true {
			recordEvent("switch_rejected", "Состояние текущего VPN требует восстановления")
			errorOut(w, 409, "Текущее VPN-соединение требует восстановления. Сначала отключите его.")
			return
		}
		if err := preflightProfile(p, req.ID); err != nil {
			recordEvent("preflight_failed", "Новый сервер не прошёл предварительную проверку; VPN не отключён")
			errorOut(w, 409, "Новый сервер не прошёл предварительную проверку. Текущее соединение сохранено.")
			return
		}
		recordEvent("switch_requested", "Новый сервер проверен; выполняется переключение")
		announceVPNConnecting(req.ID, true)
		output, e := runEngine("stop")
		if e != nil || active() || !strings.Contains(output, "NETWORK_RESTORED=YES") {
			recordEvent("switch_stop_failed", "Не удалось безопасно остановить прежнее соединение")
			announceVPNActionError("switch")
			errorOut(w, 409, "Не удалось безопасно отключить предыдущее соединение. Проверьте диагностику.")
			return
		}
	}
	if req.ID != p.Active {
		if e := selectProfileLocked(&p, req.ID); e != nil {
			recordEvent("selection_failed", "Не удалось выбрать новый профиль")
			if switching {
				announceVPNActionError("switch")
			}
			errorOut(w, 409, safeError(e))
			return
		}
	}
	if !switching {
		announceVPNConnecting(req.ID, false)
	}
	output, e := runEngine("start")
	if e != nil || !active() || status()["needsRecovery"] == true {
		recordEvent("connection_failed", "Подключение не подтвердилось; выполняется безопасный откат")
		announceVPNActionError("start")
		// The old selection is restored after a failed switch. We never
		// auto-reconnect to the old VPN: the underlying engine owns route/DNS safety.
		if active() {
			if _, stopErr := runEngine("stop"); stopErr != nil || active() {
				errorOut(w, 409, "Сбой подключения, отключение требует проверки сети. Конфигурацию не изменяли повторно.")
				return
			}
		}
		if previous != "" && previous != req.ID {
			// Profile selection writes are done only after engine shutdown.
			if rollbackErr := selectProfileLocked(&p, previous); rollbackErr != nil {
				errorOut(w, 409, "Не удалось подключиться. Сеть восстановлена, но не удалось вернуть исходный профиль.")
				return
			}
		}
		_ = output
		errorOut(w, 409, "Не удалось подключиться. Сеть восстановлена, прежний профиль сохранён. Проверьте диагностику.")
		return
	}
	_, _ = runEngine("confirm")
	if switching {
		recordEvent("switch_completed", "VPN переключён на выбранный профиль")
	} else {
		recordEvent("connected", "VPN подключён")
	}
	// Status is confirmed by the same runtime checks used for the API.
	// External IP verification is not required for the completion toast.
	announceVPNConnected(req.ID)
	jsonResponse(w, 200, map[string]any{"ok": true, "status": status(), "switched": switching})
}
func diagnostics() map[string]any {
	result := map[string]any{"status": status(), "route": "Недоступно", "dns": "Недоступно", "xray": "не проверен", "interfaces": "Недоступно"}
	commands := []struct {
		key  string
		args []string
	}{{"route", []string{"-4", "route", "show"}}, {"interfaces", []string{"-br", "link", "show"}}}
	for _, c := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b, e := exec.CommandContext(ctx, "/sbin/ip", c.args...).CombinedOutput()
		cancel()
		if e == nil {
			txt := string(b)
			if len(txt) > 2500 {
				txt = txt[:2500]
			}
			result[c.key] = txt
		}
	}
	if b, e := os.ReadFile("/etc/resolv.conf"); e == nil {
		result["dns"] = string(b)
	}
	state := status()
	result["engineVersion"] = engineVersion()
	result["dnsExplanation"] = "Системный DNS направляется на локальный DHCP-резолвер."
	if active() {
		dnsIP := "8.8.8.8"
		if b, err := os.ReadFile(path("state", "dns.ip")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			dnsIP = strings.TrimSpace(string(b))
		}
		dnsDev := "cvpn0"
		if b, err := os.ReadFile(path("state", "dns.dev")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			dnsDev = strings.TrimSpace(string(b))
		}
		result["dnsRouteExpected"] = dnsIP + " via " + dnsDev
		if dnsDev != "cvpn0" {
			result["dnsExplanation"] = "Системный DNS направляется на локальный DoH/DHCP-резолвер (" + dnsIP + ")."
		} else {
			result["dnsExplanation"] = "Системный DNS при VPN направляется через cvpn0 (" + dnsIP + ")."
		}
	}
	result["httpsState"], result["httpsDetail"] = httpsProbe()
	if state["needsRecovery"] == true {
		result["xray"] = "Требуется восстановление сети: " + state["reason"].(string)
	} else if state["connected"] == true {
		result["xray"] = "Xray и сетевые компоненты работают"
	} else {
		result["xray"] = "сессия VPN неактивна"
	}
	return result
}

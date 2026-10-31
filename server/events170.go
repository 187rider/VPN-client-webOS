package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const eventLimit = 60

// Events contain only server-authored messages. Never store raw engine output,
// VLESS URIs, subscription URLs, user credentials or client-supplied text.
type auditEvent struct {
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

var eventMu sync.Mutex

func readEvents() []auditEvent {
	b, err := os.ReadFile(path("webui", "state", "connection-events.json"))
	if err != nil || len(b) > 150000 {
		return nil
	}
	var events []auditEvent
	if json.Unmarshal(b, &events) != nil {
		return nil
	}
	if len(events) > eventLimit {
		events = events[len(events)-eventLimit:]
	}
	return events
}

func recordEvent(kind, message string) {
	eventMu.Lock()
	defer eventMu.Unlock()
	events := readEvents()
	events = append(events, auditEvent{Time: time.Now().UTC().Format(time.RFC3339), Kind: kind, Message: message})
	if len(events) > eventLimit {
		events = events[len(events)-eventLimit:]
	}
	b, err := json.Marshal(events)
	if err != nil {
		return
	}
	if err := atomic(path("webui", "state", "connection-events.json"), b, 0600); err != nil {
		// Never make VPN operations fail because diagnostic persistence failed.
		fmt.Fprintf(os.Stderr, "cvpn event write failed: %v\n", err)
	}
}

func recordEngineAction(action string, success bool) {
	if !success {
		switch action {
		case "start":
			recordEvent("start_failed", "VPN не удалось запустить")
		case "stop":
			recordEvent("stop_failed", "Не удалось подтвердить восстановление сети")
		case "confirm":
			recordEvent("confirm_failed", "Не удалось подтвердить пробное подключение")
		}
		return
	}
	switch action {
	case "start":
		recordEvent("connected", "VPN подключён")
	case "stop":
		recordEvent("disconnected", "VPN отключён; команда восстановления завершена")
	case "confirm":
		recordEvent("confirmed", "Пробное подключение подтверждено")
	}
}

func recordStartupObservation() {
	s := status()
	if s["needsRecovery"] == true {
		if out, err := runEngine("stop"); err == nil && strings.Contains(out, "NETWORK_RESTORED=YES") {
			recordEvent("startup_healed", "Остаточная сессия после перезагрузки автоматически очищена")
			return
		}
		recordEvent("startup_warning", "После запуска панели обнаружено повреждённое состояние VPN; автовмешательство отключено")
	} else if s["connected"] == true {
		recordEvent("panel_started", "Панель перезапущена; VPN-сессия обнаружена")
	} else {
		recordEvent("panel_started", "Панель управления запущена; VPN неактивен")
	}
}

func safeEventSummary() string {
	eventMu.Lock()
	defer eventMu.Unlock()
	events := readEvents()
	if len(events) == 0 {
		return "СОБЫТИЯ ПОДКЛЮЧЕНИЯ\nПока нет записей"
	}
	var b strings.Builder
	b.WriteString("СОБЫТИЯ ПОДКЛЮЧЕНИЯ (последние 20)\n")
	if len(events) > 20 {
		events = events[len(events)-20:]
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		b.WriteString(e.Time)
		b.WriteString(" · ")
		b.WriteString(e.Message)
		b.WriteByte('\n')
	}
	return b.String()
}

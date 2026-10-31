package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type autostartConfig struct {
	VPNAutoConnect   bool   `json:"vpnAutoConnect"`
	DelaySeconds     int    `json:"delaySeconds"`
	PanelHookEnabled bool   `json:"panelHookEnabled"`
	NotifyOnStartup  bool   `json:"notifyOnStartup"`
	LastAutoConnect  string `json:"lastAutoConnect,omitempty"`
	LastStatus       string `json:"lastStatus,omitempty"`
}

var (
	autostartLock sync.Mutex
)

const defaultHookPath = "/var/lib/webosbrew/init.d/90-cvpn-webos"
const defaultPayloadHook = "/media/developer/apps/usr/palm/applications/su.cvpn.webos/payload/bin/boot-hook"

func autostartSettingsFile() string {
	return path("webui", "autostart-settings.json")
}

func isHookInstalled() bool {
	fi, err := os.Lstat(defaultHookPath)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0 || fi.Mode().IsRegular()
}

func readAutostartConfig() autostartConfig {
	cfg := autostartConfig{
		VPNAutoConnect:   false,
		DelaySeconds:     15,
		PanelHookEnabled: isHookInstalled(),
		NotifyOnStartup:  true,
		LastStatus:       "не выполнялся",
	}
	data, err := os.ReadFile(autostartSettingsFile())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.DelaySeconds < 5 || cfg.DelaySeconds > 120 {
		cfg.DelaySeconds = 15
	}
	cfg.PanelHookEnabled = isHookInstalled()
	return cfg
}

func writeAutostartConfig(cfg autostartConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomic(autostartSettingsFile(), data, 0600)
}

func syncPanelHook(enabled bool) {
	if enabled {
		if !isHookInstalled() {
			hookDir := filepath.Dir(defaultHookPath)
			if _, err := os.Stat(hookDir); err == nil {
				_ = os.Symlink(defaultPayloadHook, defaultHookPath)
			}
		}
	} else {
		if isHookInstalled() {
			_ = os.Remove(defaultHookPath)
		}
	}
}

func getProfileNodeInfo(id string) (string, string) {
	p, err := readProfiles()
	if err == nil {
		for _, item := range p.Items {
			if item.ID == id {
				ip := item.Address
				if ip == "" {
					b, _ := os.ReadFile(path("config", "server-ip.txt"))
					ip = strings.TrimSpace(string(b))
				}
				return item.Name, ip
			}
		}
	}
	b, _ := os.ReadFile(path("config", "server-ip.txt"))
	return "VPN-сервер", strings.TrimSpace(string(b))
}

func appendWebAddress(msg string, en bool) string {
	if lan := tvLANAddress(); lan != "" {
		if en {
			return msg + fmt.Sprintf("\nWeb UI: http://%s:8765", lan)
		}
		return msg + fmt.Sprintf("\nПанель: http://%s:8765", lan)
	}
	return msg
}

func sendStartupNotification(msg string) bool {
	if len([]rune(msg)) > 140 {
		r := []rune(msg)
		msg = string(r[:139]) + "…"
	}
	if err := notifySystemToast(msg); err != nil {
		log.Printf("[CVPN webOS] status notification unavailable: %v", err)
		return false
	}
	return true
}

func sendStartupStatusToast(nodeName, nodeIP string, success bool, err error) {
	en := tvEN()
	nodeLabel := cleanNoticeText(nodeName, 26)
	if nodeIP != "" {
		nodeLabel += " (" + nodeIP + ")"
	}
	var msg string
	if success {
		if en {
			msg = fmt.Sprintf("CVPN · Connected: %s", nodeLabel)
		} else {
			msg = fmt.Sprintf("CVPN · Подключено: %s", nodeLabel)
		}
	} else {
		if en {
			msg = fmt.Sprintf("CVPN · Connection failed: %s", nodeLabel)
		} else {
			msg = fmt.Sprintf("CVPN · Сбой подключения: %s", nodeLabel)
		}
	}
	msg = appendWebAddress(msg, en)
	sendStartupNotification(msg)
}

func runBootAutoConnect() {
	go func() {
		cfg := readAutostartConfig()
		if !cfg.VPNAutoConnect {
			if cfg.NotifyOnStartup {
				en := tvEN()
				msg := "CVPN · VPN отключён"
				if en {
					msg = "CVPN · VPN disconnected"
				}
				msg = appendWebAddress(msg, en)
				sendStartupNotification(msg)
			}
			return
		}

		delay := time.Duration(cfg.DelaySeconds) * time.Second
		if delay < 5*time.Second {
			delay = 10 * time.Second
		}
		time.Sleep(delay)

		mx.Lock()
		p, err := readProfiles()
		if err != nil || p.Active == "" {
			mx.Unlock()
			log.Println("[CVPN webOS] Автоподключение: нет активного профиля")
			if cfg.NotifyOnStartup {
				en := tvEN()
				msg := "CVPN · Автозапуск: сервер не выбран"
				if en {
					msg = "CVPN · Autostart: no server selected"
				}
				msg = appendWebAddress(msg, en)
				sendStartupNotification(msg)
			}
			return
		}
		targetID := p.Active
		nodeName, nodeIP := getProfileNodeInfo(targetID)
		mx.Unlock()

		if active() {
			log.Println("[CVPN webOS] Автоподключение: VPN уже активен")
			if cfg.NotifyOnStartup {
				sendStartupStatusToast(nodeName, nodeIP, true, nil)
			}
			return
		}

		log.Printf("[CVPN webOS] Автоподключение VPN к узлу %s (%s)...\n", nodeName, nodeIP)

		mx.Lock()
		defer mx.Unlock()
		if active() {
			return
		}

		output, err := runEngine("start")
		nowStr := time.Now().UTC().Format(time.RFC3339)
		cfg = readAutostartConfig()
		cfg.LastAutoConnect = nowStr

		if err != nil || !active() {
			cfg.LastStatus = fmt.Sprintf("Ошибка: %s (%s)", nodeName, nodeIP)
			_ = writeAutostartConfig(cfg)
			log.Printf("[CVPN webOS] Сбой автоподключения: %v (%s)\n", err, output)
			if active() {
				_, _ = runEngine("stop")
			}
			if cfg.NotifyOnStartup {
				sendStartupStatusToast(nodeName, nodeIP, false, err)
			}
			return
		}

		_, confirmErr := runEngine("confirm")
		if confirmErr != nil {
			log.Printf("[CVPN webOS] Предупреждение: confirm после автоподключения вернул %v\n", confirmErr)
		}

		cfg.LastStatus = fmt.Sprintf("Подключено: %s (%s)", nodeName, nodeIP)
		_ = writeAutostartConfig(cfg)
		recordEvent("autostart", fmt.Sprintf("VPN подключён автоматически к узлу %s (%s)", nodeName, nodeIP))
		log.Printf("[CVPN webOS] VPN успешно подключён автоматически к %s (%s)\n", nodeName, nodeIP)

		if cfg.NotifyOnStartup {
			sendStartupStatusToast(nodeName, nodeIP, true, nil)
		}
	}()
}

func (a *app) handleAutostartStatus(w http.ResponseWriter, r *http.Request) {
	cfg := readAutostartConfig()
	p, _ := readProfiles()
	activeName, activeIP := getProfileNodeInfo(p.Active)
	webAddr := ""
	if lan := tvLANAddress(); lan != "" {
		webAddr = fmt.Sprintf("http://%s:8765", lan)
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":               true,
		"vpnAutoConnect":   cfg.VPNAutoConnect,
		"delaySeconds":     cfg.DelaySeconds,
		"panelHookEnabled": cfg.PanelHookEnabled,
		"notifyOnStartup":  cfg.NotifyOnStartup,
		"lastAutoConnect":  cfg.LastAutoConnect,
		"lastStatus":       cfg.LastStatus,
		"activeProfile":    activeName,
		"activeProfileIp":  activeIP,
		"activeProfileId":  p.Active,
		"webAddress":       webAddr,
	})
}

func (a *app) handleAutostartSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VPNAutoConnect   bool `json:"vpnAutoConnect"`
		DelaySeconds     int  `json:"delaySeconds"`
		PanelHookEnabled bool `json:"panelHookEnabled"`
		NotifyOnStartup  bool `json:"notifyOnStartup"`
	}
	if err := decodeJSON(r, &req); err != nil {
		errorOut(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}

	if req.DelaySeconds < 5 || req.DelaySeconds > 120 {
		req.DelaySeconds = 15
	}

	autostartLock.Lock()
	defer autostartLock.Unlock()

	cfg := readAutostartConfig()
	cfg.VPNAutoConnect = req.VPNAutoConnect
	cfg.DelaySeconds = req.DelaySeconds
	cfg.PanelHookEnabled = req.PanelHookEnabled
	cfg.NotifyOnStartup = req.NotifyOnStartup

	syncPanelHook(req.PanelHookEnabled)
	cfg.PanelHookEnabled = isHookInstalled()

	if err := writeAutostartConfig(cfg); err != nil {
		errorOut(w, http.StatusInternalServerError, "Не удалось сохранить настройки автозапуска")
		return
	}

	a.handleAutostartStatus(w, r)
}

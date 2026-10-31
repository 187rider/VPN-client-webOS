package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type autostartConfig struct {
	VPNAutoConnect   bool   `json:"vpnAutoConnect"`
	DelaySeconds     int    `json:"delaySeconds"`
	PanelHookEnabled bool   `json:"panelHookEnabled"`
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

func runBootAutoConnect() {
	go func() {
		cfg := readAutostartConfig()
		if !cfg.VPNAutoConnect {
			return
		}

		delay := time.Duration(cfg.DelaySeconds) * time.Second
		if delay < 5*time.Second {
			delay = 10 * time.Second
		}
		time.Sleep(delay)

		if active() {
			log.Println("[CVPN webOS] Автоподключение: VPN уже активен, пропуск")
			return
		}

		mx.Lock()
		p, err := readProfiles()
		if err != nil || p.Active == "" {
			mx.Unlock()
			log.Println("[CVPN webOS] Автоподключение: нет активного профиля")
			return
		}
		targetID := p.Active
		mx.Unlock()

		log.Printf("[CVPN webOS] Автоподключение VPN к профилю %s...\n", targetID)

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
			cfg.LastStatus = "ошибка: " + safeError(err)
			_ = writeAutostartConfig(cfg)
			log.Printf("[CVPN webOS] Сбой автоподключения: %v (%s)\n", err, output)
			if active() {
				_, _ = runEngine("stop")
			}
			return
		}

		_, confirmErr := runEngine("confirm")
		if confirmErr != nil {
			log.Printf("[CVPN webOS] Предупреждение: confirm после автоподключения вернул %v\n", confirmErr)
		}

		cfg.LastStatus = "успешно подключено"
		_ = writeAutostartConfig(cfg)
		recordEvent("autostart", "VPN подключён автоматически при старте ТВ")
		log.Println("[CVPN webOS] VPN успешно подключён автоматически")
	}()
}

func (a *app) handleAutostartStatus(w http.ResponseWriter, r *http.Request) {
	cfg := readAutostartConfig()
	p, _ := readProfiles()
	activeName := "не выбран"
	for _, item := range p.Items {
		if item.ID == p.Active {
			activeName = item.Name
			break
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":               true,
		"vpnAutoConnect":   cfg.VPNAutoConnect,
		"delaySeconds":     cfg.DelaySeconds,
		"panelHookEnabled": cfg.PanelHookEnabled,
		"lastAutoConnect":  cfg.LastAutoConnect,
		"lastStatus":       cfg.LastStatus,
		"activeProfile":    activeName,
		"activeProfileId":  p.Active,
	})
}

func (a *app) handleAutostartSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VPNAutoConnect   bool `json:"vpnAutoConnect"`
		DelaySeconds     int  `json:"delaySeconds"`
		PanelHookEnabled bool `json:"panelHookEnabled"`
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

	syncPanelHook(req.PanelHookEnabled)
	cfg.PanelHookEnabled = isHookInstalled()

	if err := writeAutostartConfig(cfg); err != nil {
		errorOut(w, http.StatusInternalServerError, "Не удалось сохранить настройки автозапуска")
		return
	}

	a.handleAutostartStatus(w, r)
}

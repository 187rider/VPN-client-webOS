package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultGeoIPURL   = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
	defaultGeoSiteURL = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
)

type geoIPConfig struct {
	AutoUpdate         bool   `json:"autoUpdate"`
	UpdateIntervalDays int    `json:"updateIntervalDays"`
	GeoIPURL           string `json:"geoipUrl"`
	GeoSiteURL         string `json:"geositeUrl"`
	LastCheck          string `json:"lastCheck,omitempty"`
	LastSuccess        string `json:"lastSuccess,omitempty"`
	LastStatus         string `json:"lastStatus,omitempty"`
}

var (
	geoIPLock       sync.Mutex
	geoIPUpdating   bool
	geoIPUpdateText string
)

func geoIPSettingsFile() string {
	return path("webui", "geoip-settings.json")
}

func readGeoIPConfig() geoIPConfig {
	cfg := geoIPConfig{
		AutoUpdate:         true,
		UpdateIntervalDays: 7,
		GeoIPURL:           defaultGeoIPURL,
		GeoSiteURL:         defaultGeoSiteURL,
		LastStatus:         "не проверялось",
	}
	data, err := os.ReadFile(geoIPSettingsFile())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.UpdateIntervalDays <= 0 {
		cfg.UpdateIntervalDays = 7
	}
	if strings.TrimSpace(cfg.GeoIPURL) == "" {
		cfg.GeoIPURL = defaultGeoIPURL
	}
	if strings.TrimSpace(cfg.GeoSiteURL) == "" {
		cfg.GeoSiteURL = defaultGeoSiteURL
	}
	return cfg
}

func writeGeoIPConfig(cfg geoIPConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomic(geoIPSettingsFile(), data, 0600)
}

func getFileInfo(filename string) (int64, string, bool) {
	fi, err := os.Stat(path("bin", filename))
	if err != nil {
		return 0, "", false
	}
	return fi.Size(), fi.ModTime().UTC().Format(time.RFC3339), true
}

func copyGeoFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func downloadGeoFile(targetURL, targetFilename string) error {
	u, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("некорректный URL: %s", targetURL)
	}

	client := &http.Client{
		Timeout: 240 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "CVPN-webOS/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка загрузки %s: %w", targetFilename, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("сервер вернул HTTP %d для %s", resp.StatusCode, targetFilename)
	}

	stagingDir := path("bin")
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		return err
	}

	stagingPath := filepath.Join(stagingDir, fmt.Sprintf(".staging-%s-%d", targetFilename, time.Now().UnixNano()))
	defer os.Remove(stagingPath)

	out, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	written, err := io.Copy(out, io.LimitReader(resp.Body, 52428800))
	_ = out.Close()
	if err != nil {
		return fmt.Errorf("ошибка записи %s: %w", targetFilename, err)
	}

	if written < 500*1024 {
		return fmt.Errorf("файл %s слишком мал (%d байт), возможно ссылка неверна", targetFilename, written)
	}

	destPath := path("bin", targetFilename)
	if err := os.Rename(stagingPath, destPath); err != nil {
		return fmt.Errorf("не удалось сохранить %s: %w", targetFilename, err)
	}

	payloadDest := filepath.Join("/media/developer/apps/usr/palm/applications/su.cvpn.webos/payload/bin", targetFilename)
	if _, err := os.Stat(filepath.Dir(payloadDest)); err == nil {
		_ = copyGeoFile(destPath, payloadDest, 0644)
	}

	return nil
}

func performGeoIPUpdate() error {
	geoIPLock.Lock()
	if geoIPUpdating {
		geoIPLock.Unlock()
		return errors.New("обновление уже выполняется")
	}
	geoIPUpdating = true
	geoIPUpdateText = "Загрузка geoip.dat..."
	geoIPLock.Unlock()

	defer func() {
		geoIPLock.Lock()
		geoIPUpdating = false
		geoIPUpdateText = ""
		geoIPLock.Unlock()
	}()

	cfg := readGeoIPConfig()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	cfg.LastCheck = nowStr

	if err := downloadGeoFile(cfg.GeoIPURL, "geoip.dat"); err != nil {
		cfg.LastStatus = fmt.Sprintf("Ошибка geoip: %v", err)
		_ = writeGeoIPConfig(cfg)
		return err
	}

	geoIPLock.Lock()
	geoIPUpdateText = "Загрузка geosite.dat..."
	geoIPLock.Unlock()

	if err := downloadGeoFile(cfg.GeoSiteURL, "geosite.dat"); err != nil {
		cfg.LastStatus = fmt.Sprintf("Ошибка geosite: %v", err)
		_ = writeGeoIPConfig(cfg)
		return err
	}

	cfg.LastSuccess = nowStr
	cfg.LastStatus = "успешно обновлено"
	_ = writeGeoIPConfig(cfg)

	return nil
}

func startGeoIPAutoUpdater() {
	go func() {
		time.Sleep(1 * time.Minute)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		checkAndUpdate := func() {
			cfg := readGeoIPConfig()
			if !cfg.AutoUpdate {
				return
			}
			interval := time.Duration(cfg.UpdateIntervalDays) * 24 * time.Hour
			if interval <= 0 {
				interval = 7 * 24 * time.Hour
			}

			shouldUpdate := false
			if cfg.LastSuccess == "" {
				shouldUpdate = true
			} else if t, err := time.Parse(time.RFC3339, cfg.LastSuccess); err == nil {
				if time.Since(t) >= interval {
					shouldUpdate = true
				}
			} else {
				shouldUpdate = true
			}

			if shouldUpdate {
				log.Println("[CVPN webOS] Запуск автоматического обновления баз GeoIP/GeoSite...")
				if err := performGeoIPUpdate(); err != nil {
					log.Printf("[CVPN webOS] Ошибка автообновления GeoIP: %v\n", err)
				} else {
					log.Println("[CVPN webOS] Базы GeoIP/GeoSite успешно обновлены автоматически")
				}
			}
		}

		checkAndUpdate()

		for range ticker.C {
			checkAndUpdate()
		}
	}()
}

func (a *app) handleGeoIPStatus(w http.ResponseWriter, r *http.Request) {
	cfg := readGeoIPConfig()
	geoipSize, geoipMod, geoipExists := getFileInfo("geoip.dat")
	geositeSize, geositeMod, geositeExists := getFileInfo("geosite.dat")

	geoIPLock.Lock()
	updating := geoIPUpdating
	updateText := geoIPUpdateText
	geoIPLock.Unlock()

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"enabled":            geoipExists && geositeExists,
		"autoUpdate":         cfg.AutoUpdate,
		"updateIntervalDays": cfg.UpdateIntervalDays,
		"geoipUrl":           cfg.GeoIPURL,
		"geositeUrl":         cfg.GeoSiteURL,
		"defaultGeoipUrl":    defaultGeoIPURL,
		"defaultGeositeUrl":  defaultGeoSiteURL,
		"geoipSize":          geoipSize,
		"geositeSize":        geositeSize,
		"geoipModified":      geoipMod,
		"geositeModified":    geositeMod,
		"lastCheck":          cfg.LastCheck,
		"lastSuccess":        cfg.LastSuccess,
		"lastStatus":         cfg.LastStatus,
		"updating":           updating,
		"updateText":         updateText,
		"vpnActive":          active(),
	})
}

func (a *app) handleGeoIPSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoUpdate         bool   `json:"autoUpdate"`
		UpdateIntervalDays int    `json:"updateIntervalDays"`
		GeoIPURL           string `json:"geoipUrl"`
		GeoSiteURL         string `json:"geositeUrl"`
	}
	if err := decodeJSON(r, &req); err != nil {
		errorOut(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}

	req.GeoIPURL = strings.TrimSpace(req.GeoIPURL)
	req.GeoSiteURL = strings.TrimSpace(req.GeoSiteURL)
	if req.GeoIPURL == "" {
		req.GeoIPURL = defaultGeoIPURL
	}
	if req.GeoSiteURL == "" {
		req.GeoSiteURL = defaultGeoSiteURL
	}
	if !strings.HasPrefix(req.GeoIPURL, "http://") && !strings.HasPrefix(req.GeoIPURL, "https://") {
		errorOut(w, http.StatusBadRequest, "Адрес GeoIP должен начинаться с http:// или https://")
		return
	}
	if !strings.HasPrefix(req.GeoSiteURL, "http://") && !strings.HasPrefix(req.GeoSiteURL, "https://") {
		errorOut(w, http.StatusBadRequest, "Адрес GeoSite должен начинаться с http:// или https://")
		return
	}
	if req.UpdateIntervalDays < 1 || req.UpdateIntervalDays > 90 {
		req.UpdateIntervalDays = 7
	}

	cfg := readGeoIPConfig()
	cfg.AutoUpdate = req.AutoUpdate
	cfg.UpdateIntervalDays = req.UpdateIntervalDays
	cfg.GeoIPURL = req.GeoIPURL
	cfg.GeoSiteURL = req.GeoSiteURL

	if err := writeGeoIPConfig(cfg); err != nil {
		errorOut(w, http.StatusInternalServerError, "Не удалось сохранить настройки")
		return
	}

	a.handleGeoIPStatus(w, r)
}

func (a *app) handleGeoIPManualUpdate(w http.ResponseWriter, r *http.Request) {
	geoIPLock.Lock()
	if geoIPUpdating {
		geoIPLock.Unlock()
		errorOut(w, http.StatusConflict, "Обновление уже выполняется")
		return
	}
	geoIPLock.Unlock()

	if err := performGeoIPUpdate(); err != nil {
		errorOut(w, http.StatusInternalServerError, safeError(err))
		return
	}

	msg := "Базы успешно обновлены"
	if active() {
		msg += ". Для применения новых правил в текущей сессии рекомендуется переподключить VPN."
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": msg,
	})
}

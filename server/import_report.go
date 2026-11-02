package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// Never return untrusted Xray errors or subscription URLs to the browser.
// URLs can include UUIDs, bearer tokens and server credentials.
type ImportIssue struct {
	Server string `json:"server"`
	Reason string `json:"reason"`
}

func newImportIssue(index int, raw, reason string) ImportIssue {
	label := fmt.Sprintf("Сервер %d", index+1)
	if u, err := url.Parse(raw); err == nil && u.Fragment != "" {
		name := strings.TrimSpace(u.Fragment)
		runes := []rune(name)
		if len(runes) <= 48 && len(runes) >= 2 && !strings.ContainsAny(name, "/\\:@?#=&%") {
			safe := true
			for _, r := range runes {
				if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '·' || r == '_' || unicode.Is(unicode.So, r)) {
					safe = false
					break
				}
			}
			if safe {
				label = name
			}
		}
	}
	return ImportIssue{Server: label, Reason: reason}
}

func importFailureReason(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "supported transports"), strings.Contains(s, "only tcp"), strings.Contains(s, "transport"):
		return "Неподдерживаемый транспорт (доступны TCP, XHTTP, gRPC и WebSocket)"
	case strings.Contains(s, "supported security"):
		return "Неподдерживаемый протокол безопасности (Reality или TLS)"
	case strings.Contains(s, "grpc") || strings.Contains(s, "servicename") || strings.Contains(s, "multimode") || strings.Contains(s, "alpn"):
		return "Неверные параметры gRPC (serviceName, режим или TLS)"
	case strings.Contains(s, "websocket") || strings.Contains(s, "wssettings"):
		return "Неверные параметры WebSocket (путь, host или TLS)"
	case strings.Contains(s, "reality") || strings.Contains(s, "pbk") || strings.Contains(s, "sni"):
		return "Параметры Reality отсутствуют или не поддерживаются"
	case strings.Contains(s, "xhttp") || strings.Contains(s, "extra"):
		return "Ошибка параметров XHTTP"
	case strings.Contains(s, "ipv4") || strings.Contains(s, "resolve") || strings.Contains(s, "dns"):
		return "Не удалось определить IPv4 сервера"
	case strings.Contains(s, "uuid") || strings.Contains(s, "port") || strings.Contains(s, "vless"):
		return "Некорректная VLESS-ссылка или адрес"
	case strings.Contains(s, "xray") || strings.Contains(s, "config"):
		return "Конфигурация отклонена проверкой Xray"
	default:
		return "Профиль не прошёл проверку совместимости"
	}
}

func summarizeImportFailure(issues []ImportIssue) string {
	if len(issues) == 0 {
		return "Нет подходящих конфигураций"
	}
	return "Ни один сервер не импортирован. " + issues[0].Reason + ". Другие серверы не изменены."
}

// duplicateProfileReason describes exactly which *within-group* profile
// matched the VLESS URI after removing only the display fragment. Never echo
// UUIDs, query parameters or subscription URLs to the mobile browser.
func duplicateProfileReason(p Profile) string {
	name := safeDuplicateName(p.Name)
	addr := net.ParseIP(strings.TrimSpace(p.Address))
	if addr != nil && addr.To4() != nil {
		return fmt.Sprintf("Совпадает с «%s» (IPv4 %s). Повтор одного профиля с тем же названием и параметрами подключения.", name, addr.String())
	}
	return fmt.Sprintf("Совпадает с «%s». Повтор одного профиля с тем же названием и параметрами подключения.", name)
}

func safeDuplicateName(name string) string {
	name = safeLabel(name, 52)
	if len([]rune(name)) < 2 || strings.Contains(name, "@") || strings.ContainsAny(name, "/\\?#=&%") {
		return "другим сервером"
	}
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || r == '-' || r == '·' || r == '_' || unicode.Is(unicode.So, r)) {
			return "другим сервером"
		}
	}
	return name
}

func duplicateInSourceReason(index int, previous string) string {
	u, err := url.Parse(previous)
	if err != nil {
		return "Повтор одной и той же записи внутри подписки"
	}
	endpoint := ""
	if addr := net.ParseIP(u.Hostname()); addr != nil && addr.To4() != nil {
		endpoint = fmt.Sprintf(" (IPv4 %s)", addr.String())
	}
	label := newImportIssue(index, previous, "").Server
	if strings.HasPrefix(label, "Сервер ") {
		return "Повтор одной VLESS-конфигурации внутри подписки" + endpoint + ""
	}
	return fmt.Sprintf("Повтор VLESS-конфигурации сервера «%s»%s внутри подписки", safeDuplicateName(label), endpoint)
}

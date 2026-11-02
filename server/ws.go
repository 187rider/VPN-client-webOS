package main

import (
	"errors"
	"net/url"
	"unicode"
)

// WebSocket is a transport (not a separate VPN protocol). The client continues
// using the installed, audited VLESS / TUN / DNS configuration template.

func safeWSText(s string, limit int, label string) (string, error) {
	if len(s) > limit {
		return "", errors.New("invalid WebSocket " + label + " length")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errors.New("invalid WebSocket " + label)
		}
	}
	return s, nil
}

func wsFromLink(q url.Values, defaultHost string) (map[string]any, error) {
	path := q.Get("path")
	if path == "" {
		path = "/"
	}
	path, err := safeWSText(path, 1024, "path")
	if err != nil {
		return nil, err
	}
	result := map[string]any{"path": path}

	host := q.Get("host")
	if host == "" {
		host = q.Get("sni")
	}
	if host == "" {
		host = defaultHost
	}
	if host != "" {
		host, err = safeWSText(host, 255, "Host header")
		if err != nil {
			return nil, err
		}
		result["headers"] = map[string]string{"Host": host}
	}
	return result, nil
}

func safeWSJSON(raw any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{"path": "/"}, nil
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("WebSocket JSON requires wsSettings object")
	}
	result := map[string]any{"path": "/"}
	if value, present := config["path"]; present {
		p, ok := value.(string)
		if !ok {
			return nil, errors.New("invalid WebSocket path type")
		}
		p, err := safeWSText(p, 1024, "path")
		if err != nil {
			return nil, err
		}
		result["path"] = p
	}
	if value, present := config["headers"]; present {
		headers, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid WebSocket headers type")
		}
		cleanHeaders := map[string]string{}
		for k, v := range headers {
			if len(k) > 64 {
				return nil, errors.New("invalid WebSocket header name length")
			}
			s, ok := v.(string)
			if !ok {
				return nil, errors.New("invalid WebSocket header value")
			}
			s, err := safeWSText(s, 512, "header value")
			if err != nil {
				return nil, err
			}
			cleanHeaders[k] = s
		}
		if len(cleanHeaders) > 0 {
			result["headers"] = cleanHeaders
		}
	}
	return result, nil
}

func safeWSTLS(raw any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("WebSocket TLS requires tlsSettings")
	}
	if value, present := config["allowInsecure"]; present && value != false {
		return nil, errors.New("insecure TLS certificates are not allowed")
	}
	result := map[string]any{}
	for _, key := range []string{"serverName", "fingerprint"} {
		if value, ok := config[key].(string); ok && value != "" {
			value, err := safeWSText(value, 255, key)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
	}
	if field, found := config["alpn"]; found {
		alpn, ok := field.([]any)
		if !ok || len(alpn) > 4 {
			return nil, errors.New("invalid WebSocket ALPN")
		}
		cleanAlpn := make([]string, 0, len(alpn))
		for _, item := range alpn {
			s, ok := item.(string)
			if !ok || s == "" {
				return nil, errors.New("invalid WebSocket ALPN item")
			}
			cleanAlpn = append(cleanAlpn, s)
		}
		if len(cleanAlpn) > 0 {
			result["alpn"] = cleanAlpn
		}
	}
	return result, nil
}

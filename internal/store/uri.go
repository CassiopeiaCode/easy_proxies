package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NameFromURI extracts node name from URI fragment (#name).
func NameFromURI(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		if idx := strings.LastIndex(raw, "#"); idx != -1 && idx < len(raw)-1 {
			return raw[idx+1:]
		}
		return ""
	}
	if u.Fragment == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(u.Fragment)
	if err == nil && decoded != "" {
		return decoded
	}
	return u.Fragment
}

type vmessJSONHostPort struct {
	Add  string      `json:"add"`
	Port interface{} `json:"port"`
}

func (v vmessJSONHostPort) portInt() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case int64:
		return int(p)
	case string:
		n, _ := net.LookupPort("tcp", strings.TrimSpace(p))
		return n
	default:
		return 0
	}
}

// HostPortFromURI extracts the host/port used as dedup primary key.
//
// Rules:
// - Prefer URL host:port (u.Hostname/u.Port).
// - Default port when missing: 443 for most TLS-like schemes; 80 for http; 1080 for socks.
// - For vmess:// base64-json format, decode and extract (add, port).
func HostPortFromURI(raw string) (host string, port int, protocol string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", errors.New("uri empty")
	}

	// Allow "URI [extra description...]" by taking the first token.
	if fields := strings.Fields(raw); len(fields) > 0 {
		raw = fields[0]
	}

	// Special-case: vmess base64-json (vmess://<base64(json)>)
	if strings.HasPrefix(strings.ToLower(raw), "vmess://") {
		encoded := strings.TrimSpace(strings.TrimPrefix(raw, "vmess://"))
		if encoded != "" && !strings.Contains(encoded, "@") {
			var decoded []byte
			if b, decErr := base64.StdEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.RawStdEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.RawURLEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.URLEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			}

			if len(decoded) > 0 {
				var v vmessJSONHostPort
				if jErr := json.Unmarshal(decoded, &v); jErr == nil {
					h := strings.TrimSpace(v.Add)
					p := v.portInt()
					if h != "" && p > 0 && p <= 65535 {
						return h, p, "vmess", nil
					}
				}
			}
		}
	}

	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", 0, "", fmt.Errorf("parse uri: %w", parseErr)
	}

	protocol = strings.ToLower(u.Scheme)
	host = strings.TrimSpace(u.Hostname())
	portStr := strings.TrimSpace(u.Port())

	if host == "" {
		return "", 0, protocol, errors.New("missing host")
	}

	if portStr == "" {
		switch protocol {
		case "http":
			port = 80
		case "socks", "socks5", "socks4", "socks4a":
			port = 1080
		default:
			port = 443
		}
		return host, port, protocol, nil
	}

	p, convErr := net.LookupPort("tcp", portStr)
	if convErr != nil {
		return "", 0, protocol, fmt.Errorf("invalid port %q: %w", portStr, convErr)
	}
	return host, p, protocol, nil
}

package auth

import (
	"net"
	"net/http"
	"strings"
)

func requestClientIP(r *http.Request, trustedProxies []string) string {
	if r == nil {
		return ""
	}

	remoteIP := remoteAddressIP(r.RemoteAddr)
	if trustedProxyContains(trustedProxies, remoteIP) {
		if ip := trustedProxyHeaderClientIP(r.Header); ip != nil {
			return ip.String()
		}
	}

	if remoteIP == nil {
		return ""
	}
	return remoteIP.String()
}

func trustedProxyHeaderClientIP(header http.Header) net.IP {
	for _, name := range []string{"CF-Connecting-IP", "True-Client-IP"} {
		if ip := firstHeaderIP(header.Values(name)); ip != nil {
			return ip
		}
	}
	if ip := firstForwardedHeaderIP(header.Values("Forwarded")); ip != nil {
		return ip
	}
	if ip := firstHeaderIP(header.Values("X-Forwarded-For")); ip != nil {
		return ip
	}
	if ip := firstHeaderIP(header.Values("X-Real-IP")); ip != nil {
		return ip
	}
	return nil
}

func firstForwardedHeaderIP(values []string) net.IP {
	for _, value := range values {
		for _, element := range strings.Split(value, ",") {
			for _, part := range strings.Split(element, ";") {
				key, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
					continue
				}
				if ip := parseHeaderIP(raw); ip != nil {
					return ip
				}
			}
		}
	}
	return nil
}

func firstHeaderIP(values []string) net.IP {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if ip := parseHeaderIP(part); ip != nil {
				return ip
			}
		}
	}
	return nil
}

func parseHeaderIP(value string) net.IP {
	raw := strings.TrimSpace(value)
	raw = strings.Trim(raw, `"`)
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "unknown") || strings.HasPrefix(raw, "_") {
		return nil
	}

	if strings.HasPrefix(raw, "[") {
		if end := strings.Index(raw, "]"); end > 0 {
			raw = raw[1:end]
		}
	} else if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}

	raw = strings.Trim(raw, "[]")
	return net.ParseIP(raw)
}

func remoteAddressIP(remoteAddr string) net.IP {
	raw := strings.TrimSpace(remoteAddr)
	if raw == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	raw = strings.Trim(raw, "[]")
	return net.ParseIP(raw)
}

func trustedProxyContains(trustedProxies []string, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, proxy := range trustedProxies {
		proxy = strings.TrimSpace(proxy)
		if proxy == "" {
			continue
		}
		if proxy == "*" {
			return true
		}
		if trustedIP := net.ParseIP(proxy); trustedIP != nil {
			if trustedIP.Equal(ip) {
				return true
			}
			continue
		}
		if _, network, err := net.ParseCIDR(proxy); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

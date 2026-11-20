// Package proxylist is the proxy list of the Python version (services/proxy_list_service.py):
// named proxies, their health, and the key that identifies one proxy written in different ways.
package proxylist

import "strings"

// pyQuote is urllib.parse.quote(value, safe=""): everything but letters, digits and "_.-~" is %XX.
func pyQuote(value string) string {
	var out strings.Builder
	for _, b := range []byte(value) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '.', b == '-', b == '~':
			out.WriteByte(b)
		default:
			out.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[b>>4])) + strings.ToUpper(string("0123456789abcdef"[b&15])))
		}
	}
	return out.String()
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// colonProxyToURL turns host:port:user:pass and host:port into http:// addresses.
func colonProxyToURL(url string) string {
	parts := strings.SplitN(url, ":", 4)
	if len(parts) == 4 && isDigits(parts[1]) {
		return "http://" + pyQuote(parts[2]) + ":" + pyQuote(parts[3]) + "@" + parts[0] + ":" + parts[1]
	}
	if len(parts) == 2 && isDigits(parts[1]) {
		return "http://" + url
	}
	return url
}

// NormalizeProxyURL is services/proxy_service.py normalize_proxy_url: colon formats become http://,
// socks:// and socks5:// become socks5h:// (remote DNS).
func NormalizeProxyURL(url string) string {
	candidate := strings.TrimSpace(url)
	if candidate != "" && !strings.Contains(candidate, "://") {
		candidate = colonProxyToURL(candidate)
	}
	lowered := strings.ToLower(candidate)
	switch {
	case strings.HasPrefix(lowered, "socks://"):
		return "socks5h://" + candidate[len("socks://"):]
	case strings.HasPrefix(lowered, "socks5://"):
		return "socks5h://" + candidate[len("socks5://"):]
	}
	return candidate
}

// Key identifies one proxy however it is written: normalized like a real request, scheme and host
// lower-cased, trailing slash dropped. Only for comparing; never written back to an account.
func Key(url string) string {
	url = strings.TrimRight(NormalizeProxyURL(url), "/")
	scheme, rest, ok := strings.Cut(url, "://")
	if !ok {
		return url
	}
	userinfo, host := "", rest
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo, host = rest[:at+1], rest[at+1:]
	}
	return strings.ToLower(scheme) + "://" + userinfo + strings.ToLower(host)
}

package cookies

import (
	"fmt"
	"strings"
)

// extractedCookie holds a cookie from browser extraction before Netscape formatting.
type extractedCookie struct {
	domain   string
	httpOnly bool
	path     string
	secure   bool
	expiry   int64
	name     string
	value    string
}

// isRelevantDomain returns true for YouTube/Google/Twitch domains (matching TS).
func isRelevantDomain(domain string) bool {
	return strings.Contains(domain, "youtube.com") ||
		strings.Contains(domain, "google.com") ||
		strings.Contains(domain, "twitch.tv")
}

// isEssentialCookie checks if a cookie should be included in extraction (matching TS).
func isEssentialCookie(name, domain string) bool {
	// YouTube essential cookies
	if essentialYouTubeCookies[name] {
		return true
	}
	// Google domain auth cookies
	if strings.Contains(domain, "google.com") {
		if name == "SID" || name == "HSID" || name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P") {
			return true
		}
	}
	// Twitch essential cookies
	if strings.Contains(domain, "twitch.tv") && essentialTwitchCookies[name] {
		return true
	}
	return false
}

// deduplicateAndFormat filters, deduplicates (preferring youtube.com over google.com),
// and formats cookies to Netscape format. Matches TS deduplicateCookies() behavior.
func deduplicateAndFormat(cookies []extractedCookie) []string {
	// Deduplicate by name, preferring youtube.com over google.com
	type entry struct {
		cookie extractedCookie
		domain string
	}
	byName := make(map[string]entry)
	// Preserve order
	var order []string

	for _, c := range cookies {
		if !isRelevantDomain(c.domain) {
			continue
		}
		if !isEssentialCookie(c.name, c.domain) {
			continue
		}

		existing, exists := byName[c.name]
		if exists && strings.Contains(existing.domain, "youtube.com") && !strings.Contains(c.domain, "youtube.com") {
			continue // Already have youtube.com version, skip google.com
		}
		if !exists {
			order = append(order, c.name)
		}
		byName[c.name] = entry{cookie: c, domain: c.domain}
	}

	var lines []string
	for _, name := range order {
		e := byName[name]
		c := e.cookie

		subdomain := "FALSE"
		if strings.HasPrefix(c.domain, ".") {
			subdomain = "TRUE"
		}
		secureStr := "FALSE"
		if c.secure {
			secureStr = "TRUE"
		}
		prefix := ""
		if c.httpOnly {
			prefix = "#HttpOnly_"
		}

		lines = append(lines, fmt.Sprintf("%s%s\t%s\t%s\t%s\t%d\t%s\t%s",
			prefix, c.domain, subdomain, c.path, secureStr, c.expiry, c.name, c.value))
	}
	return lines
}

// mergeCookieFiles merges existing and new Netscape cookie strings.
// New cookies take priority over existing ones with the same name+domain.
func mergeCookieFiles(existing, newCookies string) string {
	type cookieKey struct {
		name   string
		domain string
	}

	// Parse a Netscape cookie file into ordered keys and a map
	parseCookies := func(content string) ([]cookieKey, map[cookieKey]string) {
		keys := make([]cookieKey, 0)
		m := make(map[cookieKey]string)
		for _, line := range strings.Split(content, "\n") {
			trimmed := line
			// Handle #HttpOnly_ prefix
			if strings.HasPrefix(trimmed, "#HttpOnly_") {
				trimmed = strings.TrimPrefix(trimmed, "#HttpOnly_")
			} else if strings.HasPrefix(trimmed, "#") || strings.TrimSpace(trimmed) == "" {
				continue
			}
			fields := strings.Split(trimmed, "\t")
			if len(fields) < 7 {
				continue
			}
			domain := fields[0]
			name := fields[5]
			k := cookieKey{name: name, domain: domain}
			if _, exists := m[k]; !exists {
				keys = append(keys, k)
			}
			m[k] = line
		}
		return keys, m
	}

	existingKeys, existingMap := parseCookies(existing)
	_, newMap := parseCookies(newCookies)

	// Start with existing cookies, overwrite with new ones
	merged := make(map[cookieKey]string)
	for k, v := range existingMap {
		merged[k] = v
	}
	allKeys := make([]cookieKey, 0, len(existingKeys))
	allKeys = append(allKeys, existingKeys...)

	for k, v := range newMap {
		if _, exists := merged[k]; !exists {
			allKeys = append(allKeys, k)
		}
		merged[k] = v
	}

	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service")
	lines = append(lines, "")
	for _, k := range allKeys {
		if line, ok := merged[k]; ok {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

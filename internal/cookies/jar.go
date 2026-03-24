// Package cookies provides Netscape cookie file parsing, SAPISIDHASH generation,
// and cookie management for YouTube and Twitch authentication.
package cookies

import (
	"crypto/sha1"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CookieJar parses and manages cookies from a Netscape-format cookie file.
type CookieJar struct {
	mu       sync.RWMutex
	cookies  map[string]string // name -> value
	domains  map[string]string // name -> domain (for dedup priority)
	filePath string
}

// Essential YouTube cookies needed for authentication.
var essentialYouTubeCookies = map[string]bool{
	"SAPISID": true, "__Secure-1PAPISID": true, "__Secure-3PAPISID": true,
	"SID": true, "HSID": true, "SSID": true, "APISID": true,
	"__Secure-1PSID": true, "__Secure-3PSID": true,
	"__Secure-1PSIDTS": true, "__Secure-3PSIDTS": true,
	"__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true,
	"LOGIN_INFO": true, "VISITOR_INFO1_LIVE": true, "VISITOR_PRIVACY_METADATA": true,
	"YSC": true, "__Secure-ROLLOUT_TOKEN": true, "CONSENT": true, "PREF": true,
}

// Essential Twitch cookies.
var essentialTwitchCookies = map[string]bool{
	"auth-token": true, "twilight-user": true, "login": true, "name": true,
}

// NewCookieJar creates an empty CookieJar.
func NewCookieJar() *CookieJar {
	return &CookieJar{
		cookies: make(map[string]string),
		domains: make(map[string]string),
	}
}

// Load reads and parses a Netscape cookie file.
func (j *CookieJar) Load(filePath string) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.filePath = filePath
	j.cookies = make(map[string]string)
	j.domains = make(map[string]string)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No cookies file is OK
		}
		return fmt.Errorf("failed to read cookie file: %w", err)
	}

	lines := strings.SplitSeq(string(data), "\n")
	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip comments, but NOT #HttpOnly_ lines (those are data)
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}

		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue // Malformed line — Netscape format requires exactly 7 tab-delimited fields
		}

		rawDomain := strings.TrimSpace(parts[0])
		domain := rawDomain
		if after, ok := strings.CutPrefix(domain, "#HttpOnly_"); ok {
			domain = after
		}

		name := strings.TrimSpace(parts[5])
		value := strings.TrimSpace(parts[6])

		// Skip entries with empty domain or name
		if domain == "" || name == "" {
			continue
		}

		// Only include YouTube/Google and Twitch cookies
		isYouTubeGoogle := strings.Contains(domain, "youtube.com") || strings.Contains(domain, "google.com")
		isTwitch := strings.Contains(domain, "twitch.tv")

		if !isYouTubeGoogle && !isTwitch {
			continue
		}

		// Filter to essential cookies
		isGoogleAuth := strings.Contains(domain, "google.com") && (name == "SID" || name == "HSID" ||
			name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P"))
		isTwitchEssential := isTwitch && essentialTwitchCookies[name]

		if !essentialYouTubeCookies[name] && !isGoogleAuth && !isTwitchEssential {
			continue
		}

		// Prefer youtube.com cookies over google.com when both exist
		existingDomain, exists := j.domains[name]
		if exists && strings.Contains(existingDomain, "youtube.com") && !strings.Contains(domain, "youtube.com") {
			continue
		}

		j.cookies[name] = value
		j.domains[name] = domain
	}

	return nil
}

// Reload reloads cookies from the same file.
func (j *CookieJar) Reload() error {
	j.mu.RLock()
	path := j.filePath
	j.mu.RUnlock()

	if path == "" {
		return nil
	}
	return j.Load(path)
}

// GetCookie returns a cookie value by name.
func (j *CookieJar) GetCookie(name string) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.cookies[name]
}

// cookieValueSanitizer strips characters that could enable header injection.
var cookieValueSanitizer = strings.NewReplacer("\r", "", "\n", "")

// sanitizeCookieValue strips characters that could enable header injection.
func sanitizeCookieValue(v string) string {
	return cookieValueSanitizer.Replace(v)
}

// GetCookieHeader returns a Cookie header string with all cookies.
func (j *CookieJar) GetCookieHeader() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	pairs := make([]string, 0, len(j.cookies))
	for name, value := range j.cookies {
		pairs = append(pairs, name+"="+sanitizeCookieValue(value))
	}
	return strings.Join(pairs, "; ")
}

// HasYouTubeAuthCookies returns true if SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO are present.
func (j *CookieJar) HasYouTubeAuthCookies() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()

	hasSapisid := j.cookies["SAPISID"] != "" || j.cookies["__Secure-3PAPISID"] != ""
	hasLoginInfo := j.cookies["LOGIN_INFO"] != ""
	return hasSapisid && hasLoginInfo
}

// GetSapisid returns the SAPISID cookie value, falling back to __Secure-3PAPISID.
func (j *CookieJar) GetSapisid() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if v := j.cookies["SAPISID"]; v != "" {
		return v
	}
	return j.cookies["__Secure-3PAPISID"]
}

// GetSapisidCookies returns all SAPISID variants.
func (j *CookieJar) GetSapisidCookies() (sapisid, sapisid1p, sapisid3p string) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	sapisid = j.cookies["SAPISID"]
	if sapisid == "" {
		sapisid = j.cookies["__Secure-3PAPISID"]
	}
	sapisid1p = j.cookies["__Secure-1PAPISID"]
	sapisid3p = j.cookies["__Secure-3PAPISID"]
	return
}

// GenerateAuthorizationHeader generates the full Authorization header with SAPISIDHASH variants.
func (j *CookieJar) GenerateAuthorizationHeader(origin string) string {
	sapisid, sapisid1p, sapisid3p := j.GetSapisidCookies()

	// Capture timestamp once so all SID hashes in the same header share the same time
	now := time.Now().Unix()
	var parts []string

	if sapisid != "" {
		parts = append(parts, makeSidAuthorization("SAPISIDHASH", sapisid, origin, now))
	}
	if sapisid1p != "" {
		parts = append(parts, makeSidAuthorization("SAPISID1PHASH", sapisid1p, origin, now))
	}
	if sapisid3p != "" {
		parts = append(parts, makeSidAuthorization("SAPISID3PHASH", sapisid3p, origin, now))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " ")
}

func makeSidAuthorization(scheme, sid, origin string, unixTime int64) string {
	timestamp := strconv.FormatInt(unixTime, 10)
	hashInput := timestamp + " " + sid + " " + origin
	hash := fmt.Sprintf("%x", sha1.Sum([]byte(hashInput)))
	return fmt.Sprintf("%s %s_%s", scheme, timestamp, hash)
}

// GetTwitchAuthToken returns the Twitch auth-token cookie value.
func (j *CookieJar) GetTwitchAuthToken() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.cookies["auth-token"]
}

// HasTwitchAuthCookies returns true if the Twitch auth-token is present.
func (j *CookieJar) HasTwitchAuthCookies() bool {
	return j.GetTwitchAuthToken() != ""
}

// IsEmpty returns true if no cookies are loaded.
func (j *CookieJar) IsEmpty() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.cookies) == 0
}

// GetFilePath returns the path to the cookie file.
func (j *CookieJar) GetFilePath() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.filePath
}

// Package cookies provides Netscape cookie file parsing, SAPISIDHASH generation,
// and cookie management for YouTube and Twitch authentication.
package cookies

import (
	"crypto/sha1"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cookieJarLogger is the optional logger interface CookieJar uses to report
// malformed or skipped cookie lines. Kept optional so NewCookieJar callers
// that don't want diagnostics need not supply a logger.
type cookieJarLogger interface {
	Debug(msg string, args ...any)
}

// CookieJar parses and manages cookies from a Netscape-format cookie file.
type CookieJar struct {
	mu       sync.RWMutex
	cookies  map[string]string // name -> value
	domains  map[string]string // name -> domain (for dedup priority)
	filePath string
	logger   cookieJarLogger // optional; set via SetLogger
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

// SetLogger attaches an optional logger that CookieJar uses to report
// diagnostic events like malformed lines in the cookie file. Safe to call
// before or after Load. Pass nil to clear.
func (j *CookieJar) SetLogger(logger cookieJarLogger) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.logger = logger
}

// Load reads and parses a Netscape cookie file.
//
// The file is fully read and parsed into a new set of maps before the jar's
// live state is replaced. This way a transient read error (EIO, permission
// flip) cannot silently wipe authentication that was valid a moment ago —
// either the load fully succeeds and swaps in the new maps, or the previous
// state is left intact. A not-exist file is still treated as an empty jar.
func (j *CookieJar) Load(filePath string) error {
	// Snapshot logger once; the field is protected by the mutex.
	j.mu.RLock()
	logger := j.logger
	j.mu.RUnlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No cookies file is OK; clear state so callers see an empty jar.
			j.mu.Lock()
			j.filePath = filePath
			j.cookies = make(map[string]string)
			j.domains = make(map[string]string)
			j.mu.Unlock()
			return nil
		}
		return fmt.Errorf("failed to read cookie file: %w", err)
	}

	cookies := make(map[string]string)
	domains := make(map[string]string)

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
			// Malformed line — Netscape format requires exactly 7 tab-delimited fields.
			// Log at Debug so the file can be diagnosed without being spammy when
			// third-party tools leave trailing blank lines or stray comments.
			if logger != nil {
				logger.Debug("skipping malformed cookie line", "fields", len(parts))
			}
			continue
		}

		rawDomain := strings.TrimSpace(parts[0])
		domain := rawDomain
		if after, ok := strings.CutPrefix(domain, "#HttpOnly_"); ok {
			domain = after
		}

		name := strings.TrimSpace(parts[5])
		// Join tab-separated remainder so a cookie whose value legitimately
		// contains a tab (rare, but permitted by some emitters) is preserved
		// instead of truncated to the first tab.
		value := strings.TrimSpace(strings.Join(parts[6:], "\t"))

		// Skip entries with empty domain or name
		if domain == "" || name == "" {
			continue
		}

		// Only include YouTube/Google and Twitch cookies. Use suffix-anchored
		// matchers so a malicious or hand-edited file entry like
		// ".fakegoogle.com.evil.tld" does not get treated as google.com.
		isYouTubeGoogle := isYouTubeDomain(domain) || isGoogleDomain(domain)
		isTwitch := isTwitchDomain(domain)

		if !isYouTubeGoogle && !isTwitch {
			continue
		}

		// Filter to essential cookies
		isGoogleAuth := isGoogleDomain(domain) && (name == "SID" || name == "HSID" ||
			name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P"))
		isTwitchEssential := isTwitch && essentialTwitchCookies[name]

		if !essentialYouTubeCookies[name] && !isGoogleAuth && !isTwitchEssential {
			continue
		}

		// Prefer youtube.com cookies over google.com when both exist
		existingDomain, exists := domains[name]
		if exists && isYouTubeDomain(existingDomain) && !isYouTubeDomain(domain) {
			continue
		}

		cookies[name] = value
		domains[name] = domain
	}

	j.mu.Lock()
	j.filePath = filePath
	j.cookies = cookies
	j.domains = domains
	j.mu.Unlock()

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
// cookieValueSanitizer strips characters that could enable header injection
// or silently corrupt the Cookie: header parser:
//   - \r / \n: classic header-injection vectors.
//   - ; , : cookie header field separators per RFC 6265; a value containing
//     these terminates the pair early and mis-routes everything after.
var cookieValueSanitizer = strings.NewReplacer("\r", "", "\n", "", ";", "", ",", "")

// sanitizeCookieValue strips characters that could enable header injection.
func sanitizeCookieValue(v string) string {
	return cookieValueSanitizer.Replace(v)
}

// GetCookieHeader returns a Cookie header string with all cookies.
//
// Pairs are emitted in a stable sorted-by-name order. Go's map iteration is
// deliberately randomized, which meant two successive calls could produce
// different Cookie headers for the same jar contents — a poor fit for
// SAPISIDHASH flows where an attacker-controlled reshuffle makes HTTP-level
// debugging painful and occasionally trips YouTube endpoints that inspect
// __Secure-* ordering. Alphabetical is simple and deterministic.
func (j *CookieJar) GetCookieHeader() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+sanitizeCookieValue(j.cookies[name]))
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

// allowedSAPISIDHASHOrigins is the set of origins for which Moombox will
// generate a SAPISIDHASH Authorization header. Keeping this tight is
// defense-in-depth: if a bug or config path ever lets a caller supply an
// attacker-controlled origin (e.g. "https://evil.example"), we must not
// hand them a valid SAPISIDHASH bound to that origin — Google's auth uses
// the origin as a shared secret between client and server. The current
// in-tree callers only pass https://www.youtube.com, so tightening to an
// allowlist is zero-regression.
var allowedSAPISIDHASHOrigins = map[string]struct{}{
	"https://www.youtube.com":    {},
	"https://youtube.com":        {},
	"https://studio.youtube.com": {},
	"https://music.youtube.com":  {},
	"https://www.youtubekids.com": {},
}

// GenerateAuthorizationHeader generates the full Authorization header with SAPISIDHASH variants.
// If origin is not a recognized YouTube origin, returns "" — callers must not
// use the result to authenticate against arbitrary origins.
func (j *CookieJar) GenerateAuthorizationHeader(origin string) string {
	if _, ok := allowedSAPISIDHASHOrigins[origin]; !ok {
		return ""
	}
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

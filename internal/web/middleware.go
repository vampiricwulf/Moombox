package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// CORSMiddleware validates Origin headers based on network_access config.
func CORSMiddleware(store *config.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			var networkAccess string
			store.Read(func(c *config.MoomboxConfig) {
				networkAccess = c.Network.NetworkAccess
			})

			if origin != "" {
				// Validate origin based on network_access
				if isAllowedOrigin(origin, networkAccess) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
				}
			}

			if r.Method == http.MethodOptions {
				if origin != "" && isAllowedOrigin(origin, networkAccess) {
					w.WriteHeader(http.StatusNoContent)
				} else {
					// Audit Q-8: include Allow + Access-Control-Max-Age:0 on
					// preflight rejection so callers get a usable response
					// (advertise the methods we *would* accept) and browsers
					// don't cache the failure for the default 5s preflight
					// window — without Max-Age:0 a transient origin
					// misconfiguration takes effect for several seconds even
					// after the user fixes it.
					w.Header().Set("Allow", "GET, POST, PUT, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Max-Age", "0")
					w.WriteHeader(http.StatusForbidden)
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders adds security-related response headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Strict-Transport-Security: emitted only when the connection is
		// already over TLS so that first-time http:// visitors never see
		// this header (which would pin them to HTTPS for an origin that
		// might not have a valid cert). 1-year max-age matches common
		// guidance; includeSubDomains is intentionally omitted because
		// Moombox does not own subdomains of the host it's bound to.
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}

		// Permissions-Policy
		w.Header().Set("Permissions-Policy",
			"accelerometer=(), "+
				"autoplay=(self), "+
				"camera=(), "+
				"clipboard-write=(self), "+
				"encrypted-media=(self), "+
				"geolocation=(), "+
				"gyroscope=(), "+
				"microphone=(), "+
				"picture-in-picture=(self)")

		// Content Security Policy
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"font-src 'self' https://cdn.jsdelivr.net; "+
				"img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.jsdelivr.net https://fonts.gstatic.com; "+
				"connect-src 'self' ws: wss: https://cdn.jsdelivr.net data:; "+
				"frame-src https://www.youtube-nocookie.com https://player.twitch.tv; "+
				"object-src 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")

		next.ServeHTTP(w, r)
	})
}

// CSRFMiddleware validates Origin/Referer headers on mutating requests.
// Same-process clients (TUI) bypass CSRF by sending the internal token.
//
// Policy (tightened per audit reports/web.md C-1/C-5/C-8): every mutating
// request must present either (a) the in-process InternalToken, or
// (b) an Origin/Referer header that matches the configured network_access
// policy. The previous "missing Origin OK on loopback" bypass allowed any
// local process or same-origin browser tab on default-config installs to
// issue POST /api/restart, PUT /api/auth/set-password, etc. without proof
// of browser context.
//
// Legitimate browser fetches always set Origin on cross-origin OR
// same-origin mutating requests (Fetch spec). Non-browser local CLIs
// (e.g. `moombox add`) should set Origin to the server's base URL or use
// the InternalToken.
func CSRFMiddleware(store *config.Store, internalToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only check mutating methods
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			// Exempt loopback-only POT provider endpoints from CSRF.
			// These are called by yt-dlp (Python scripts) which don't send
			// Origin/Referer headers. The routes themselves enforce LoopbackOnly,
			// so CSRF protection is redundant.
			p := r.URL.Path
			if p == "/get_pot" || p == "/invalidate_caches" || p == "/invalidate_it" {
				next.ServeHTTP(w, r)
				return
			}

			// Same-process clients (TUI) send the internal token to bypass CSRF.
			// Browsers cannot set custom headers cross-origin without a CORS
			// preflight, which the server does not grant, so this is safe.
			// Use constant-time comparison to prevent timing side-channels.
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(InternalTokenHeader)), []byte(internalToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}

			var networkAccess string
			store.Read(func(c *config.MoomboxConfig) {
				networkAccess = c.Network.NetworkAccess
			})

			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = r.Header.Get("Referer")
			}

			// Mutating requests must present a recognizable Origin or Referer.
			// This replaces the previous loopback bypass that let any local
			// process call state-changing endpoints without browser context.
			if origin == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Forbidden: missing origin"}`))
				return
			}

			if !isAllowedOrigin(origin, networkAccess) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Forbidden: invalid origin"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// IPGateMiddleware restricts access based on network_access config.
func IPGateMiddleware(store *config.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ExtractIP(r)

			var networkAccess string
			store.Read(func(c *config.MoomboxConfig) {
				networkAccess = c.Network.NetworkAccess
			})

			switch networkAccess {
			case "external", "public":
				// Allow all
			case "lan":
				// Allow loopback + private IPs
				if !isLoopback(ip) && !isPrivateIP(ip) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			default: // "localhost" or unset
				if !isLoopback(ip) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// LoopbackOnly is a middleware that restricts to loopback addresses only.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ExtractIP(r)
		if !isLoopback(ip) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"Forbidden: loopback only"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAllowedOrigin validates an origin URL against the network_access config.
// Uses proper URL parsing instead of substring matching.
func isAllowedOrigin(origin, networkAccess string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	hostname := u.Hostname()
	if hostname == "" {
		return false
	}

	switch networkAccess {
	case "localhost":
		return isLoopback(hostname) || hostname == "localhost"
	case "lan":
		return isLoopback(hostname) || hostname == "localhost" || isPrivateIP(hostname)
	case "external", "public":
		return true
	default:
		return isLoopback(hostname) || hostname == "localhost"
	}
}

// ExtractIP gets the client's real IP from the request.
// Does NOT trust X-Forwarded-For to prevent spoofing attacks.
// Exported so route handlers can use it without duplicating the logic.
func ExtractIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr == "localhost"
	}
	return ip.IsLoopback()
}

// privateCIDRs is parsed once at package init to avoid re-parsing on every HTTP request.
var privateCIDRs = []*net.IPNet{
	mustParseCIDR("10.0.0.0/8"),
	mustParseCIDR("172.16.0.0/12"),
	mustParseCIDR("192.168.0.0/16"),
	mustParseCIDR("fc00::/7"), // IPv6 private
}

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	// Link-local addresses (fe80::/10 IPv6, 169.254.0.0/16 IPv4)
	// Phones on LAN often connect via IPv6 link-local
	if ip.IsLinkLocalUnicast() {
		return true
	}

	for _, cidr := range privateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isLocalIP returns true for loopback or private IP addresses.
func isLocalIP(ipStr string) bool {
	return isLoopback(ipStr) || isPrivateIP(ipStr)
}

// IsLoopbackRequest returns true if the request is from a loopback address.
func IsLoopbackRequest(r *http.Request) bool {
	return isLoopback(ExtractIP(r))
}

// IsLocalOrPrivateRequest returns true if the request is from a loopback or
// private (LAN) address. Used by auth endpoints to match the server's
// AuthMiddleware trust policy which allows both loopback and private IPs.
func IsLocalOrPrivateRequest(r *http.Request) bool {
	return isLocalIP(ExtractIP(r))
}

// shouldSkipCompression returns true for paths where compression should be
// skipped (e.g., video streaming endpoints that use range requests).
func shouldSkipCompression(p string) bool {
	return strings.HasPrefix(p, "/api/jobs/") && strings.HasSuffix(p, "/video")
}

// MaxBodySize limits request body size for mutating methods to prevent
// abuse. Individual endpoints (e.g., /import at 500MB) can override by
// wrapping req.Body with their own http.MaxBytesReader.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead &&
				r.Method != http.MethodOptions && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

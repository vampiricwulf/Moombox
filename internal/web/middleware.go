package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// CORSMiddleware validates Origin headers based on network_access config.
func CORSMiddleware(cfg *config.MoomboxConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" {
				// Validate origin based on network_access
				if isAllowedOrigin(origin, cfg.Network.NetworkAccess) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
				}
			}

			if r.Method == http.MethodOptions {
				if origin != "" && isAllowedOrigin(origin, cfg.Network.NetworkAccess) {
					w.WriteHeader(http.StatusNoContent)
				} else {
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
				"connect-src 'self' https://cdn.jsdelivr.net data:; "+
				"frame-src https://www.youtube-nocookie.com https://player.twitch.tv; "+
				"object-src 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")

		next.ServeHTTP(w, r)
	})
}

// CSRFMiddleware validates Origin/Referer headers on mutating requests.
// Same-process clients (TUI) bypass CSRF by sending the internal token.
func CSRFMiddleware(cfg *config.MoomboxConfig, internalToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only check mutating methods
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			// Same-process clients (TUI) send the internal token to bypass CSRF.
			// Browsers cannot set custom headers cross-origin without a CORS
			// preflight, which the server does not grant, so this is safe.
			if r.Header.Get(InternalTokenHeader) == internalToken {
				next.ServeHTTP(w, r)
				return
			}

			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = r.Header.Get("Referer")
			}

			// If Origin/Referer present, validate it
			if origin != "" {
				if !isAllowedOrigin(origin, cfg.Network.NetworkAccess) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(`{"error":"Forbidden: invalid origin"}`))
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// No Origin/Referer and no internal token: reject when external
			// access with auth is enabled (prevents CSRF from form submissions
			// that omit Origin). Local/LAN access is safe since IP gate +
			// auth middleware provide sufficient protection.
			if (cfg.Network.NetworkAccess == "external" || cfg.Network.NetworkAccess == "public") && cfg.Network.PasswordHash != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Forbidden: missing origin"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// IPGateMiddleware restricts access based on network_access config.
func IPGateMiddleware(cfg *config.MoomboxConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractIP(r)

			switch cfg.Network.NetworkAccess {
			case "external", "public":
				// Allow all
			case "lan":
				// Allow loopback + private IPs
				if !isLoopback(ip) && !isPrivateIP(ip) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			default: // "local" or unset
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
		ip := extractIP(r)
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
	case "local":
		return isLoopback(hostname) || hostname == "localhost"
	case "lan":
		return isLoopback(hostname) || hostname == "localhost" || isPrivateIP(hostname)
	case "external", "public":
		return true
	default:
		return isLoopback(hostname) || hostname == "localhost"
	}
}

// extractIP gets the client's real IP from the request.
// Does NOT trust X-Forwarded-For to prevent spoofing attacks.
func extractIP(r *http.Request) string {
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
	return isLoopback(extractIP(r))
}

// IsLocalOrPrivateRequest returns true if the request is from a loopback or
// private (LAN) address. Used by auth endpoints to match the server's
// AuthMiddleware trust policy which allows both loopback and private IPs.
func IsLocalOrPrivateRequest(r *http.Request) bool {
	return isLocalIP(extractIP(r))
}

// SkipCompression can be used to check if compression should be skipped
// for certain paths (e.g., video streaming).
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


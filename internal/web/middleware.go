package web

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// CORSMiddleware validates Origin headers based on network_access config.
func CORSMiddleware(store *config.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Decided ONCE: the preflight branch below used to re-run the same
			// comparison, and the two must never be able to disagree.
			allowed := false
			if origin != "" {
				allowed, _ = originAllowed(store, r, origin)
			}

			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
			}

			if r.Method == http.MethodOptions {
				if allowed {
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
				"img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.betterttv.net https://cdn.7tv.app https://cdn.frankerfacez.com https://cdn.jsdelivr.net https://fonts.gstatic.com; "+
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
func CSRFMiddleware(store *config.Store, internalToken string, logger interface {
	Warn(msg string, args ...any)
}) func(http.Handler) http.Handler {
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

			allowed, comparedHost := originAllowed(store, r, origin)
			if !allowed {
				// One line naming the pair that was compared. The most common
				// cause of a 403 here is a reverse proxy that rewrites Host
				// without being listed in network.trusted_proxies, and without
				// this the operator sees only the browser's console error
				// (Arc 5 Task 1 follow-up). The value is client-chosen, so it
				// is clipped before it reaches the dashboard's log panel. This
				// middleware runs ahead of the IP gate and of every per-route
				// rate limiter, so an unauthenticated peer can fire this line
				// once per request; volume is bounded only by the logger's
				// fixed-size ring buffer and its file rotation
				// (internal/logger/logger.go).
				logger.Warn("CSRF: origin refused",
					"origin", clipForLog(origin),
					"host", clipForLog(comparedHost))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Forbidden: invalid origin"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ipAllowedByNetworkAccess applies the network_access policy to the
// request's source IP. Shared by IPGateMiddleware (routed requests) and the
// WebSocket upgrade interception in Server.Start, which bypasses the
// middleware chain entirely.
func ipAllowedByNetworkAccess(store *config.Store, r *http.Request) bool {
	ip := EffectiveClientIP(store, r)

	var networkAccess string
	store.Read(func(c *config.MoomboxConfig) {
		networkAccess = c.Network.NetworkAccess
	})

	switch networkAccess {
	case "external", "public":
		return true
	case "lan":
		return isLoopback(ip) || isPrivateIP(ip)
	default: // "localhost" or unset
		return isLoopback(ip)
	}
}

// IPGateMiddleware restricts access based on network_access config.
func IPGateMiddleware(store *config.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ipAllowedByNetworkAccess(store, r) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
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

// originAllowed is the ONE Origin decision. CORSMiddleware, CSRFMiddleware and
// the WebSocket upgrade all route through it, so the three can never again
// disagree about which authority the request answers as (X-Forwarded-Host from
// a trusted proxy, else r.Host), how ports compare (exactly, once either side
// names one), or which hostnames a certificate attests. Before this, the
// upgrade read r.Host only and wildcarded the port, so a Host-rewriting proxy
// loaded the dashboard and then had every socket refused (Arc 5 arc-close F6).
//
// Returns the authority the origin was compared against as well, so a refusal
// can name the pair without recomputing it.
func originAllowed(store *config.Store, r *http.Request, origin string) (bool, string) {
	var networkAccess string
	store.Read(func(c *config.MoomboxConfig) {
		networkAccess = c.Network.NetworkAccess
	})
	host := effectiveRequestHost(store, r)
	return isAllowedOrigin(origin, networkAccess, host, effectiveRequestScheme(r), identityHosts()), host
}

// clipForLog bounds a header value the CLIENT chose before it reaches the log
// ring buffer the dashboard renders, and drops any invalid UTF-8 the byte cut
// may have left behind.
func clipForLog(s string) string {
	const maxLoggedHeader = 200
	if len(s) > maxLoggedHeader {
		s = s[:maxLoggedHeader] + "…"
	}
	return strings.ToValidUTF8(s, "")
}

// identityHosts returns the certificate-attested hostnames for this deployment,
// or nil when no certificate is loaded (no TLS at all — which is every
// reverse-proxy deployment) or the only one is Moombox's placeholder.
//
// Reads the CurrentCertSANs singleton LoadOrGenerateTLSConfig publishes before
// the listener starts (internal/web/tls.go); tests swap it around one case.
func identityHosts() []string {
	if CurrentCertSANs == nil {
		return nil
	}
	return CurrentCertSANs.IdentitySANs()
}

// hostInSANs reports whether hostname — an Origin's url.Hostname(), so never
// bracketed and never carrying a port — is one of the certificate-attested
// names. Both sides go through splitAuthority, so IP SANs compare canonically
// ("::1" == "0:0:0:0:0:0:0:1") and DNS SANs compare case-insensitively.
//
// allowWildcard gates the "*." expansion below (RFC 6125: exactly one
// leftmost, non-empty, dot-free label). Only the external/public arm of
// isAllowedOrigin passes true: there, sameSiteOrigin already pins hostname to
// the browser's address bar first, so the wildcard can only ever NARROW which
// same-host requests still pass. The localhost/lan/default arms have no such
// conjunction — hostInSANs alone decides — so a wildcard there would let ANY
// sibling of an operator's wildcard certificate (e.g. a stale or
// attacker-registered subdomain under *.example.com) become an allowed
// cross-origin request against a loopback-only install (fix-round-1 review
// Finding 1 / probe P7). Passing false there means only a LITERAL SAN widens
// those arms — an install with a real certificate for "dash.lan" keeps its
// socket, but "evil.example.com" does not ride in on "*.example.com".
func hostInSANs(hostname string, sans []string, allowWildcard bool) bool {
	h, _ := splitAuthority(hostname)
	if h == "" {
		return false
	}
	for _, san := range sans {
		s, _ := splitAuthority(san)
		if s == "" {
			continue
		}
		if s == h {
			return true
		}
		if !allowWildcard {
			continue
		}
		if suffix, ok := strings.CutPrefix(s, "*"); ok && strings.HasPrefix(suffix, ".") {
			if label, found := strings.CutSuffix(h, suffix); found && label != "" &&
				!strings.Contains(label, ".") {
				return true
			}
		}
	}
	return false
}

// isAllowedOrigin validates an origin URL against the network_access config.
// Uses proper URL parsing instead of substring matching.
//
// effectiveHost / effectiveScheme describe the request the origin arrived on
// (see effectiveRequestHost / effectiveRequestScheme). They are consulted ONLY
// by the external/public arm: those two policies have no IP class left to test
// an origin against — every address is admissible — so the only meaningful
// question is whether the page that issued the request was served by THIS
// deployment. Answering "yes, always" (the pre-sweep behaviour) let any page a
// LAN browser had open drive the dashboard cross-origin WITH credentials,
// because AuthMiddleware waives loopback and private peers regardless of mode:
// POST /api/restart, DELETE /api/jobs/{id}, PUT /api/config (sweep T1-6).
//
// identity is the certificate-attested host list (identityHosts). On
// external/public it is an ADDITIONAL requirement, never a substitute:
// sameSiteOrigin must still pass, and then the origin's host must also appear
// in the certificate. That conjunction is what closes DNS rebinding — a
// rebinding page controls r.Host and its own Origin, so the pre-existing arm
// compares two values it chose, but "attacker.dns" is in no certificate
// Moombox holds (Arc 5 residual M-8). An install with no certificate, or only
// Moombox's placeholder, has an empty identity and keeps the old behaviour
// exactly; a rebinding attacker who also controls DNS for a name the
// certificate attests is out of scope.
//
// localhost and lan keep their IP-class rules and gain identity as a pure
// WIDENING: both arms reject every DNS name, and the WebSocket upgrade now
// routes through this function, so without it an install holding a real
// certificate for "dash.lan" would lose the socket it has today.
func isAllowedOrigin(origin, networkAccess, effectiveHost, effectiveScheme string, identity []string) bool {
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
		return isLoopback(hostname) || hostname == "localhost" || hostInSANs(hostname, identity, false)
	case "lan":
		return isLoopback(hostname) || hostname == "localhost" || isPrivateIP(hostname) ||
			hostInSANs(hostname, identity, false)
	case "external", "public":
		if !sameSiteOrigin(origin, effectiveHost, effectiveScheme) {
			return false
		}
		if len(identity) == 0 {
			return true
		}
		return hostInSANs(hostname, identity, true)
	default:
		return isLoopback(hostname) || hostname == "localhost" || hostInSANs(hostname, identity, false)
	}
}

// effectiveRequestHost returns the authority this server answers as, for the
// purpose of the same-origin comparison: r.Host normally, or X-Forwarded-Host
// when the DIRECT peer is listed in network.trusted_proxies. Same trust rule as
// EffectiveClientIP — a client-forged header never counts, because an untrusted
// peer's headers are not consulted at all.
//
// Only the FIRST entry is ever used, which is the X-Forwarded-Host convention:
// the leftmost value names the host the ORIGINAL client asked for. That is the
// opposite end from X-Forwarded-For, whose right-to-left walk is what defeats a
// forged prefix — none of that reasoning transfers to a header read left to
// right. Joining Header.Values and cutting at the first comma is therefore
// EQUIVALENT to Header.Get plus the same cut; it is written this way only so the
// answer cannot depend on whether a proxy appended by extending the first field
// line or by adding a second one.
//
// The residual, stated plainly: a proxy listed in trusted_proxies that does not
// itself set or overwrite X-Forwarded-Host lets its peer choose the host the
// Origin is compared against. A browser cannot reach that path —
// X-Forwarded-Host is not CORS-safelisted, so setting it cross-origin needs a
// preflight this server refuses — and a non-browser client that can set
// arbitrary headers could already pass the check by sending Origin equal to
// r.Host. Configure the proxy to set the header; do not rely on its absence.
func effectiveRequestHost(store *config.Store, r *http.Request) string {
	if loadTrustedProxies(store).contains(ExtractIP(r)) {
		xfh := strings.Join(r.Header.Values("X-Forwarded-Host"), ",")
		first, _, _ := strings.Cut(xfh, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	return r.Host
}

// effectiveRequestScheme is "https" when the connection is TLS, or when
// network.trust_forwarded_proto is on and the proxy said so. IsRequestSecure
// owns that policy (internal/web/auth.go) — do not restate it here.
func effectiveRequestScheme(r *http.Request) string {
	if IsRequestSecure(r) {
		return "https"
	}
	return "http"
}

// splitAuthority splits "host", "host:port", "[::1]" or "[::1]:774" into a
// canonical hostname and its EXPLICIT port ("" when none was written). IPv6
// literals lose their brackets and are re-rendered through net.IP, so "[::1]"
// and "[0:0:0:0:0:0:0:1]" compare equal; hostnames are lowercased.
func splitAuthority(authority string) (host, port string) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(authority); err == nil {
		host, port = h, p
	} else {
		host = authority
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), port
	}
	return strings.ToLower(host), port
}

// defaultedPort fills in a scheme's default port for an authority that wrote
// none.
func defaultedPort(port, scheme string) string {
	if port != "" {
		return port
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// sameSiteOrigin reports whether `origin` (an Origin or Referer value) names
// the very host this request was addressed to.
//
// Hosts must match. Ports must match too — but ONLY when at least one side
// wrote one. A TLS-terminating reverse proxy forwards `Host: dash.example` with
// no port while the browser sends `Origin: https://dash.example`, and Moombox
// cannot know the public scheme unless trust_forwarded_proto is on; demanding a
// port match there would 403 every such deployment on its own dashboard. Two
// portless authorities therefore compare by host alone — which gives up
// nothing, because a browser sets Host from the address bar and an attacker's
// page cannot change it. The moment either side names a port, both are
// defaulted from their OWN scheme and compared exactly.
func sameSiteOrigin(origin, effectiveHost, effectiveScheme string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	oHost, oPort := splitAuthority(u.Host)
	rHost, rPort := splitAuthority(effectiveHost)
	if oHost == "" || rHost == "" || oHost != rHost {
		return false
	}
	if oPort == "" && rPort == "" {
		return true
	}
	return defaultedPort(oPort, u.Scheme) == defaultedPort(rPort, effectiveScheme)
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

// trustedProxySet is the parsed form of network.trusted_proxies, cached so
// the per-request path doesn't re-parse CIDR strings. Rebuilt lazily whenever
// the raw config value changes, which makes the setting hot-reloadable with
// no restart hook.
type trustedProxySet struct {
	raw  string       // joined source entries — the cache key
	nets []*net.IPNet // parsed entries; bare IPs become /32 or /128
}

var trustedProxyCache atomic.Pointer[trustedProxySet]

func loadTrustedProxies(store *config.Store) *trustedProxySet {
	// Clone inside the callback: copying only the slice header would leave the
	// join and the parse loop below reading ELEMENTS with no lock held. Every
	// writer today replaces the backing array rather than editing in place, so
	// that is safe by accident; cloning makes it safe by construction. Strings
	// are immutable, so a shallow clone is enough.
	var entries []string
	store.Read(func(c *config.MoomboxConfig) {
		entries = slices.Clone(c.Network.TrustedProxies)
	})
	raw := strings.Join(entries, ",")
	if cached := trustedProxyCache.Load(); cached != nil && cached.raw == raw {
		return cached
	}
	set := &trustedProxySet{raw: raw}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				e = fmt.Sprintf("%s/%d", e, bits)
			}
		}
		// Invalid entries were dropped by config validation; skip any
		// stragglers rather than trusting garbage.
		if _, n, err := net.ParseCIDR(e); err == nil {
			set.nets = append(set.nets, n)
		}
	}
	trustedProxyCache.Store(set)
	return set
}

func (s *trustedProxySet) contains(ipStr string) bool {
	if s == nil || len(s.nets) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// EffectiveClientIP returns the address every trust decision (network_access
// gate, auth skip, rate-limit keying) treats as the client. Identical to
// ExtractIP unless the DIRECT peer is listed in network.trusted_proxies —
// only then is X-Forwarded-For consulted, walking right-to-left past trusted
// hops to the first address the proxy chain did not vouch for. A client-
// forged XFF header never matters: either the direct peer is untrusted (the
// header is ignored) or the trusted proxy appended the client's real address
// to the right of the forgery.
//
// Loopback-gated surfaces (LoopbackOnly, IsLoopbackRequest, first-time
// password setup, the setup wizard) intentionally keep using the direct peer
// address: "arrived over this machine's loopback interface" is a physical-
// access signal that a forwarded header must never confer.
func EffectiveClientIP(store *config.Store, r *http.Request) string {
	direct := ExtractIP(r)
	proxies := loadTrustedProxies(store)
	if !proxies.contains(direct) {
		return direct
	}
	// Values+join, never Get: Header.Get returns only the FIRST field line and
	// Go never joins repeated headers. Proxies are free to append by adding a
	// second "X-Forwarded-For:" line instead of extending the first — HAProxy's
	// `option forwardfor` does exactly that by default, which is why its own
	// docs tell operators to read the LAST occurrence. Reading only the first
	// line there would hand the walk the client's forged entry and never show
	// it the address the proxy actually observed: a forged private address
	// would pass the lan gate and skip auth, failing OPEN. Concatenating every
	// field line in wire order (RFC 7230 §3.2.2) makes both append styles
	// equivalent; a single-line header joins to itself unchanged.
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return direct
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		hop := canonicalizeForwardedIP(parts[i])
		if !proxies.contains(hop) {
			// First hop the chain didn't vouch for. An unparseable value is
			// returned as-is on purpose rather than falling back to the
			// proxy's own private address: canonicalizeForwardedIP guarantees
			// its result never names a trusted class unless it is a genuine
			// IP in that class, so isLoopback/isPrivateIP both reject it and
			// downstream fails CLOSED (treated as a non-local client).
			return hop
		}
	}
	// Every listed hop is itself a trusted proxy — the connection originated
	// inside the trusted set (health checks, proxy self-calls).
	return direct
}

// canonicalizeForwardedIP normalizes one X-Forwarded-For entry: surrounding
// whitespace, an optional port, IPv6 brackets, and zone suffixes are
// stripped, and the address is re-rendered in its canonical form.
//
// Guarantee relied on by EffectiveClientIP: the result NEVER names a trusted
// class (loopback or private) unless it is a genuine, parseable IP in that
// class. An X-Forwarded-For entry is an address by definition, so anything
// that parses as neither IPv4 nor IPv6 is untrusted garbage; such entries
// come back trimmed-but-verbatim — preserving their diagnostic value in log
// lines — except when the verbatim text would itself be resolved to a
// trusted class downstream, in which case it is neutralized first.
func canonicalizeForwardedIP(entry string) string {
	e := strings.TrimSpace(entry)
	if e == "" {
		return e
	}
	if host, _, err := net.SplitHostPort(e); err == nil {
		e = host
	}
	e = strings.Trim(e, "[]")
	if i := strings.IndexByte(e, '%'); i >= 0 {
		e = e[:i]
	}
	if ip := net.ParseIP(e); ip != nil {
		return ip.String()
	}
	verbatim := strings.TrimSpace(entry)
	if namesTrustedClass(verbatim) {
		// Not an address, but a name a classifier resolves to a trusted
		// class. Prefixing keeps the operator-facing diagnostic ("who sent
		// this?") while guaranteeing the result parses as no IP and spells
		// no trusted class, so it fails CLOSED like any other garbage.
		return "invalid-" + verbatim
	}
	return verbatim
}

// namesTrustedClass reports whether a non-IP string would be resolved to a
// trusted class by a downstream classifier. isLoopback deliberately treats
// the bare hostname "localhost" as loopback — that special case is correct
// and load-bearing for the Origin checks in isAllowedOrigin — which makes
// "localhost" the one value a forwarded entry must never be allowed to
// spell. Compared case-insensitively so the guard does not hinge on a
// classifier's choice of case folding.
func namesTrustedClass(s string) bool {
	return strings.EqualFold(s, "localhost")
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

// IsLocalOrPrivateRequest returns true if the request's effective client IP
// (X-Forwarded-For-aware when the direct peer is a trusted proxy) is loopback
// or private. Used by auth endpoints to match the server's AuthMiddleware
// trust policy, which allows both loopback and private clients.
func IsLocalOrPrivateRequest(store *config.Store, r *http.Request) bool {
	return isLocalIP(EffectiveClientIP(store, r))
}

// shouldSkipCompression returns true for paths where compression should be
// skipped (e.g., video streaming endpoints that use range requests).
func shouldSkipCompression(p string) bool {
	return strings.HasPrefix(p, "/api/jobs/") && strings.HasSuffix(p, "/video")
}

// MaxBodySize limits request body size for mutating methods to prevent
// abuse. The import endpoint is exempt: it applies its own 500MB
// http.MaxBytesReader, and MaxBytesReader wrappers NEST rather than override
// — wrapping an already-1MB-limited body with a 500MB limit still errors
// after 1MB, which would cap every real import upload.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/import" {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead &&
				r.Method != http.MethodOptions && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

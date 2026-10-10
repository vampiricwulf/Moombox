// Package web provides the HTTP server and WebSocket handler for Moombox.
package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// InternalTokenHeader is the header name used by same-process clients (TUI)
// to bypass CSRF checks. The value must match the token generated at startup.
const InternalTokenHeader = "X-Internal-Token"

const maxCompressBodySize = 1 << 20 // 1MB

// Server is the Moombox HTTP server.
type Server struct {
	configStore    *config.Store // Authoritative cfg + mutex (DECISIONS #8)
	cfg            *config.MoomboxConfig
	router         chi.Router
	server         *http.Server
	redirectServer atomic.Pointer[http.Server] // cross-scheme redirect server, for graceful shutdown
	ws             *WebSocketHub
	auth           *AuthService
	shutdownOnce   sync.Once // Ensures shutdown logic runs only once
	internalToken  string    // Random secret for same-process CSRF bypass
	commit         string    // Build commit hash for cache busting (e.g. "abc1234")
	// assetETags memoises the content hash of each embedded asset, keyed by
	// its FS path. Only consulted when the build commit is unknown — a
	// commit-stamped build answers every asset from one string.
	assetETags  sync.Map
	loginHTML   []byte           // Cached login.html for inline serving (matches TS serveLoginPage)
	indexHTML   []byte           // Dashboard shell with cache-busted asset URLs; see serveIndex
	wsHandler   http.HandlerFunc // WebSocket upgrade handler (intercepts upgrades on any path)
	OpenBrowser bool             // Open browser to dashboard URL on start (matches TS openBrowser option)
	actualPort  atomic.Int32     // Actual bound port after Start (may differ from cfg if probed)
	tlsActive   atomic.Bool      // whether Start's listener serves HTTPS (set with actualPort)
	draining    atomic.Bool      // Set by StartDrain to make new requests 503 (audit cmd-moombox C-main:165-166)

	// ClientTokenCheck validates a persistent client token and returns a fresh session token.
	// Called by AuthMiddleware when the session cookie is missing/invalid.
	// Returns (valid, newSessionToken).
	ClientTokenCheck func(rawToken, ip string) (bool, string)

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewServer creates a new HTTP server. The Store carries both the
// *MoomboxConfig pointer and the synchronising mutex used by route +
// middleware handlers (DECISIONS #8).
func NewServer(store *config.Store, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Server {
	r := chi.NewRouter()

	// Generate a random internal token for same-process CSRF bypass (TUI).
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	s := &Server{
		configStore:   store,
		cfg:           store.Config(),
		router:        r,
		ws:            NewWebSocketHub(logger),
		internalToken: token,
		logger:        logger,
	}

	// The WebSocket upgrade path makes its own auth-skip decision — it must
	// resolve the same effective client IP as the middleware chain, or a
	// trusted reverse proxy would re-open the auth bypass there.
	s.ws.ClientIP = func(r *http.Request) string { return EffectiveClientIP(store, r) }
	// ...and class it by the same mode-aware rule AuthMiddleware waives by.
	s.ws.LocalPeer = func(ip string) bool { return isLocalPeer(store, ip) }

	// ...and the same Origin decision: before this the upgrade read r.Host
	// only and wildcarded the port, so a Host-rewriting reverse proxy loaded
	// the dashboard and then had every socket refused (Arc 5 arc-close F6).
	s.ws.OriginCheck = func(r *http.Request) (bool, string) {
		return originAllowed(store, r, r.Header.Get("Origin"))
	}

	// Apply middleware (order matters).
	// RequestID first so RecoveryMiddleware (and any future logger
	// middleware) can correlate log lines back to the originating request
	// (audit reports/web.md S-22).
	r.Use(chimiddleware.RequestID)
	// DrainMiddleware short-circuits with 503 once StartDrain is called
	// — placed BEFORE RecoveryMiddleware so the 503 path can't be
	// disturbed by a panic in a later middleware. Audit
	// reports/cmd-moombox.md C-main:165-166.
	r.Use(s.DrainMiddleware)
	r.Use(RecoveryMiddleware(logger))
	// The IP and Host gates come before CSRF: CSRF logs every refused origin,
	// and a peer the IP gate refuses must not be able to fill the log (and
	// every dashboard it is broadcast to) with lines it chose.
	r.Use(IPGateMiddleware(store))
	r.Use(HostGateMiddleware(store))
	r.Use(CORSMiddleware(store))
	r.Use(SecurityHeaders)
	r.Use(CSRFMiddleware(store, token, logger))
	r.Use(MaxBodySize(maxCompressBodySize)) // default body limit (import endpoint overrides to 500MB)
	r.Use(CompressionMiddleware)

	return s
}

// InternalToken returns the secret token that same-process clients (TUI) must
// send in the X-Internal-Token header to bypass CSRF checks.
func (s *Server) InternalToken() string {
	return s.internalToken
}

// SetCommit sets the build commit hash used for cache-busting static asset URLs.
func (s *Server) SetCommit(c string) {
	s.commit = c
}

// ActualPort returns the port the listener actually bound, or 0 when the bind
// has not completed. 0 is the PENDING sentinel, not a port: every caller
// (cmd/moombox/main.go's startup banner and its 500 ms readiness select,
// routes_wiring.go's plugin-port lookup) tests `> 0` and falls back to the
// configured port until it turns positive. Start writes it from its own
// goroutine while main.go reads it across that select window, so it is atomic
// rather than a plain int (sweep T4-35). int32 is deliberate: a TCP port never
// exceeds 65535.
func (s *Server) ActualPort() int { return int(s.actualPort.Load()) }

// TLSActive reports whether the listener Start bound serves HTTPS. Like the
// bind address, the scheme is fixed at boot: network.https_enabled saved
// later takes effect at the next restart, so a local client that must reach
// THIS listener asks here rather than reading the setting. Meaningful once
// ActualPort is non-zero.
func (s *Server) TLSActive() bool { return s.tlsActive.Load() }

// setActualPort records the bound port. Called once, by Start.
func (s *Server) setActualPort(port int) { s.actualPort.Store(int32(port)) }

// SetAuth sets the auth service for authentication middleware.
func (s *Server) SetAuth(auth *AuthService) {
	s.auth = auth
}

// AuthMiddleware checks authentication for external connections.
// Loopback and LAN clients skip auth. External clients need a valid session
// when a password is configured.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := EffectiveClientIP(s.configStore, r)

		var networkAccess, passwordHash string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			networkAccess = c.Network.NetworkAccess
			passwordHash = c.Network.PasswordHash
		})

		// Loopback and private IPs skip auth (100.64.0.0/10 is private on
		// lan only — isPrivateIPFor)
		if isLocalIPFor(ip, networkAccess) {
			next.ServeHTTP(w, r)
			return
		}

		// No auth required if not configured
		if !IsAuthRequired(networkAccess, passwordHash) {
			next.ServeHTTP(w, r)
			return
		}

		// Unauthenticated paths (login page and the two files it loads, auth
		// endpoints, POT read-only, favicon).
		//
		// /boot-theme.js and /login.js are here because this middleware answers
		// every OTHER path with login.html itself: without them the browser
		// would be handed an HTML document for the login page's own script and
		// the form would never wire up (sweep 2 row #91). Both are static,
		// credential-free assets.
		p := r.URL.Path
		if p == "/api/auth/login" ||
			p == "/api/auth/status" ||
			p == "/ping" || p == "/minter_cache" ||
			p == "/favicon.svg" || p == "/login.html" ||
			p == "/boot-theme.js" || p == "/login.js" {
			next.ServeHTTP(w, r)
			return
		}

		// Check session cookie
		if s.auth != nil {
			if cookie, err := r.Cookie("moombox_session"); err == nil {
				if valid, slid := s.auth.ValidateSessionAndSlide(cookie.Value); valid {
					// Sliding-window renewal: when the server-side TTL
					// was just refreshed past the half-elapsed mark,
					// re-issue the cookie with a fresh Max-Age so the
					// browser doesn't drop it before the server would.
					// Audit reports/web.md S-3.
					if slid {
						SetSessionCookie(w, r, cookie.Value)
					}
					next.ServeHTTP(w, r)
					return
				}
			}
		}

		// Fallback: check persistent client token cookie
		if s.ClientTokenCheck != nil {
			if cookie, err := r.Cookie("moombox_client"); err == nil && cookie.Value != "" {
				if valid, sessionToken := s.ClientTokenCheck(cookie.Value, ip); valid {
					SetSessionCookie(w, r, sessionToken)
					next.ServeHTTP(w, withSessionCookie(r, sessionToken))
					return
				}
			}
		}

		// Unauthenticated — 401 JSON for API, serve login.html for browser
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Authentication required"}`))
		} else {
			// Browser request — serve login page inline (matches TS serveLoginPage,
			// preserves the URL bar instead of redirecting)
			if s.loginHTML != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Write(s.loginHTML)
			} else {
				http.Redirect(w, r, "/login.html", http.StatusFound)
			}
		}
	})
}

// Router returns the chi router for route registration.
func (s *Server) Router() chi.Router {
	return s.router
}

// WebSocket returns the WebSocket hub.
func (s *Server) WebSocket() *WebSocketHub {
	return s.ws
}

// SetWebSocketHandler installs a WebSocket upgrade handler that intercepts
// upgrade requests on any path. This matches the TypeScript noServer mode
// where the frontend connects to ws://host/ (root path).
func (s *Server) SetWebSocketHandler(handler http.HandlerFunc) {
	s.wsHandler = handler
}

// MountStaticFiles serves embedded static assets with SPA fallback.
// The staticFS should be the sub-FS pointing to the "public" directory.
func (s *Server) MountStaticFiles(staticFS fs.FS) {
	fileServer := http.FileServer(http.FS(staticFS))

	// The dashboard shell, read once, with cache-busted asset URLs.
	//
	// The ?v= substitution is gated on trustedCommit, not merely on a non-empty
	// commit: an untrusted commit does not identify the bytes (see
	// trustedCommit), so "?v=unknown" and "?v=<rev>-dirty" name nothing.
	// Omitting them also keeps serveIndex's ETag honest — with no substitution
	// the served bytes ARE the embedded file assetETag hashes.
	s.indexHTML, _ = fs.ReadFile(staticFS, "index.html")
	if s.indexHTML != nil && s.trustedCommit() {
		suffix := "?v=" + s.commit
		s.indexHTML = bytes.ReplaceAll(s.indexHTML, []byte(`"/moombox.css"`), []byte(`"/moombox.css`+suffix+`"`))
		s.indexHTML = bytes.ReplaceAll(s.indexHTML, []byte(`"/app.js"`), []byte(`"/app.js`+suffix+`"`))
	}

	// Cache login.html for auth middleware inline serving (matches TS serveLoginPage)
	s.loginHTML, _ = fs.ReadFile(staticFS, "login.html")

	s.router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		// Don't serve SPA for API or WebSocket paths
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			http.NotFound(w, r)
			return
		}

		// Try serving the actual file first
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if urlPath == "" {
			urlPath = "index.html"
		}

		// The root and /index.html serve the SUBSTITUTED copy, not the raw
		// embedded file the FileServer below would find. Those two paths are
		// how the dashboard is actually loaded, so they are exactly the ones
		// that need the cache-busted asset URLs (Arc 5 arc-close F8).
		if urlPath == "index.html" && s.indexHTML != nil {
			s.serveIndex(w, r, staticFS)
			return
		}

		// Check if the file exists in the embedded FS
		if f, err := staticFS.Open(urlPath); err == nil {
			f.Close()
			s.staticCacheHeaders(w, r, staticFS, urlPath)
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback: serve the same shell for non-file routes.
		if s.indexHTML != nil {
			s.serveIndex(w, r, staticFS)
			return
		}

		http.NotFound(w, r)
	})
}

// serveIndex writes the dashboard shell — the copy MountStaticFiles built —
// for the root, for /index.html, and for every SPA route.
//
// no-cache, never immutable: the shell names the ?v= of the build it belongs
// to, so a cached copy would keep pointing browsers at the PREVIOUS build's
// assets. The ETag makes that revalidation cost 304 bytes; it is the build
// commit on a trusted build, and the embedded file's content hash otherwise —
// correct in both arms because an untrusted build substitutes nothing, so the
// bytes served are the bytes hashed.
//
// Content-Type is set explicitly rather than left to ServeContent's extension
// lookup, which consults the Windows registry and can be overridden there. The
// zero modtime suppresses Last-Modified, which embed.FS could not supply
// anyway; ServeContent answers If-None-Match and HEAD against the ETag.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if tag := s.assetETag(fsys, "index.html"); tag != "" {
		w.Header().Set("ETag", tag)
	}
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexHTML))
}

// trustedCommit reports whether s.commit identifies the served bytes uniquely
// — the one question both cache decisions below turn on.
//
// Two spellings do not. "unknown" is what cmd/moombox/main.go's
// resolveBuildCommit falls back to when a build carries no -ldflags stamp and
// no vcs.revision, so every such build shares it. A "-dirty" suffix means the
// build came from a working tree with uncommitted changes, so successive
// rebuilds at the same revision serve different app.js under the same string.
// Either way, keying a cache entry on it serves the PREVIOUS build's bytes.
func (s *Server) trustedCommit() bool {
	return s.commit != "" && s.commit != "unknown" && !strings.HasSuffix(s.commit, "-dirty")
}

// staticCacheHeaders sets the caching policy for one embedded asset.
//
// A URL carrying a TRUSTED build's ?v= cache-buster names a body that cannot
// change, so it is immutable for a year. Everything else must revalidate — and
// gets an ETag so the revalidation costs 304 bytes instead of the whole file.
// The previous policy keyed off the EXTENSION, which pinned unversioned
// /favicon.svg for a year and left every .js/.css re-downloading in full
// (embed.FS reports a zero ModTime, so ETag is the only validator available
// here). Sweep T2-18.
//
// The immutable branch asks trustedCommit, not just "is there a ?v=": an
// immutable entry cannot be revalidated at all for a year, so pinning one
// against a commit that does NOT identify the bytes (see trustedCommit) is
// strictly worse than the stale-ETag case the same predicate already guards in
// assetETag — a hard reload would be the only way out. Sweep R5-4/F2.
func (s *Server) staticCacheHeaders(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	if r.URL.Query().Get("v") != "" && s.trustedCommit() {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if tag := s.assetETag(fsys, name); tag != "" {
		// net/http's serveContent reads this header to answer If-None-Match,
		// so setting it before ServeHTTP is all the 304 handling needed.
		w.Header().Set("ETag", tag)
	}
}

// assetETag returns the validator for one embedded asset: the build commit
// when trustedCommit vouches for it (every asset of a build shares it — an
// ETag is scoped to its URL, so one string per build invalidates exactly the
// right things), else the file's SHA-256, hashed once per path and memoised.
func (s *Server) assetETag(fsys fs.FS, name string) string {
	if s.trustedCommit() {
		return `"` + s.commit + `"`
	}
	if v, ok := s.assetETags.Load(name); ok {
		return v.(string)
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:]) + `"`
	s.assetETags.Store(name, tag)
	return tag
}

// interceptUpgrades wraps router so a WebSocket upgrade on any path goes to
// wsHandler (matches TS noServer mode) and every other request to router.
// Split out of Start so the gates it re-applies are under test.
func interceptUpgrades(store *config.Store, router http.Handler, wsHandler http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			// The upgrade path bypasses the router's middleware chain —
			// re-apply the IP gate here, or a non-private client against
			// a "lan"-mode deployment would get the live broadcast
			// stream (job titles, logs, state) that every HTTP route
			// 403s, with only the forgeable Origin check in its way.
			if !ipAllowedByNetworkAccess(store, r) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			// And the external/public host rule HostGateMiddleware
			// applies: the rebinding page's socket would otherwise carry
			// the live stream its GETs are refused (externalHostRefused).
			if externalHostRefused(store, r) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			wsHandler(w, r)
			return
		}
		router.ServeHTTP(w, r)
	})
}

// serverHandler is the http.Server's handler: the router, with WebSocket
// upgrades taken ahead of it (interceptUpgrades) when a handler for them is
// installed, all of it inside outermostRecovery.
func (s *Server) serverHandler() http.Handler {
	var handler http.Handler = s.router
	if s.wsHandler != nil {
		handler = interceptUpgrades(s.configStore, s.router, s.wsHandler)
	}
	return outermostRecovery(s.logger, handler)
}

// Start begins listening for HTTP connections.
func (s *Server) Start(ctx context.Context) error {
	port := s.cfg.Network.Port
	if port <= 0 {
		port = 774
	}

	// Bind to localhost unless LAN/external is enabled
	host := "127.0.0.1"
	switch s.cfg.Network.NetworkAccess {
	case "lan", "external", "public":
		host = "0.0.0.0"
	}

	addr := fmt.Sprintf("%s:%d", host, port)

	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.serverHandler(),
		ReadHeaderTimeout: 30 * time.Second, // Protects against slowloris; clears deadline after headers are read
		WriteTimeout:      0,                // Disable for WebSocket and video streaming
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // Suppress HTTP server errors from stdout/stderr (routed through app logger via middleware)
	}

	// Configure TLS if enabled
	scheme := "http"
	var tlsConfig *tls.Config
	if s.cfg.Network.HTTPSEnabled {
		certPath, keyPath := resolveTLSPaths(s.cfg)

		var err error
		tlsConfig, err = LoadOrGenerateTLSConfig(certPath, keyPath, s.cfg.Network.NetworkAccess, s.logger)
		if err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}
		s.server.TLSConfig = tlsConfig
		scheme = "https"
	}

	s.logger.Info("web server starting", "addr", addr, "scheme", scheme)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("shutdown goroutine panic", "panic", r)
			}
		}()
		<-ctx.Done()
		s.doShutdown()
	}()

	// Try the preferred port, then probe nearby ports
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.logger.Warn("port in use, probing nearby ports", "port", port)
		for offset := 1; offset <= 10; offset++ {
			candidate := fmt.Sprintf("%s:%d", host, port+offset)
			ln, err = net.Listen("tcp", candidate)
			if err == nil {
				s.logger.Info("using alternative port", "port", port+offset)
				break
			}
		}
		if err != nil {
			return fmt.Errorf("port %d (and nearby ports) already in use: %w", port, err)
		}
	}

	// Cross-scheme redirect on the single configured port: each accepted
	// connection's first byte is sniffed (a TLS handshake always starts
	// with 0x16) and mismatched-scheme traffic is answered with a 307 to
	// the right scheme instead of Go's bare protocol-mismatch error text.
	//   - HTTPS enabled:  plain http:// requests redirect to https://.
	//   - HTTPS disabled: https:// requests redirect to http:// — possible
	//     only when a certificate pair exists on disk (typically left from
	//     an earlier https_enabled run) to terminate the handshake; with no
	//     loadable cert the listener stays plain-HTTP-only as before.
	var redirectLn net.Listener
	redirectScheme := ""
	if tlsConfig != nil {
		tlsRaw, plainRaw := newSchemeMux(ln, s.logger)
		ln = tls.NewListener(tlsRaw, tlsConfig)
		redirectLn, redirectScheme = plainRaw, "https"
	} else if redirTLS := loadRedirectTLSConfig(s.cfg); redirTLS != nil {
		tlsRaw, plainRaw := newSchemeMux(ln, s.logger)
		ln = plainRaw
		redirectLn, redirectScheme = tls.NewListener(tlsRaw, redirTLS), "http"
	}
	if redirectLn != nil {
		s.logger.Info("cross-scheme redirect active", "redirectsTo", redirectScheme)
		go s.serveSchemeRedirect(redirectLn, redirectScheme)
	}

	// Log the actual URL (matches TS: "Web dashboard available at ...")
	actualPort := ln.Addr().(*net.TCPAddr).Port
	s.tlsActive.Store(tlsConfig != nil)
	s.setActualPort(actualPort)
	url := fmt.Sprintf("%s://localhost:%d", scheme, actualPort)
	s.logger.Info(fmt.Sprintf("[Moombox] Web dashboard available at %s", url))
	// Mirrors the host switch above: all three of these bind 0.0.0.0.
	if s.cfg.Network.NetworkAccess == "lan" || s.cfg.Network.NetworkAccess == "external" || s.cfg.Network.NetworkAccess == "public" {
		s.logger.Info(fmt.Sprintf("[WebServer] LAN access enabled (listening on %s)", host))
	}
	if (s.cfg.Network.NetworkAccess == "external" || s.cfg.Network.NetworkAccess == "public") && s.cfg.Network.PasswordHash != "" && !s.cfg.Network.HTTPSEnabled {
		s.logger.Warn("[WebServer] External access with authentication over plain HTTP — session cookies are not encrypted. Consider setting https_enabled = true or using a reverse proxy with HTTPS.")
	}
	// Warn-boot half of the passwordless-external policy. Every interactive
	// surface (TUI settings, config API, setup wizard) refuses to SET this
	// combination, so reaching it means a hand-edited config file. Deliberately
	// a warning and not a hard failure: an existing deployment fronted by an
	// authenticating reverse proxy must keep booting.
	if (s.cfg.Network.NetworkAccess == "external" || s.cfg.Network.NetworkAccess == "public") && s.cfg.Network.PasswordHash == "" {
		s.logger.Warn("[WebServer] SECURITY: network_access is \"" + s.cfg.Network.NetworkAccess + "\" with NO dashboard password — the dashboard accepts every IP that can reach this port, unauthenticated. Set a dashboard password or lower network_access; only leave this if an authenticating reverse proxy is the ONLY route to the port.")
	}

	// Open browser to dashboard URL (matches TS openBrowser behavior)
	if s.OpenBrowser {
		openBrowserURL(url)
	}

	if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// doShutdown performs the actual shutdown logic, guarded by sync.Once.
func (s *Server) doShutdown() {
	s.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.server != nil {
			s.server.Shutdown(ctx)
		}
		// Drain the cross-scheme redirect server too, rather than letting it
		// drop in-flight redirects when the shared listener closes.
		if rs := s.redirectServer.Load(); rs != nil {
			rs.Shutdown(ctx)
		}
		s.ws.Close()
		s.logger.Info("web server stopped")
	})
}

// StartDrain marks the server as draining so any new HTTP request gets a
// clean 503 + Retry-After before the listener is closed. Used by the
// restart dispatcher (cmd/moombox/services.go) so a setup-wizard POST
// in flight gets to finish while a re-attempted browser refresh sees
// "server restarting" rather than a connection reset. Audit
// reports/cmd-moombox.md C-main:165-166.
func (s *Server) StartDrain() {
	s.draining.Store(true)
}

// DrainMiddleware returns 503 + Retry-After for any request that arrives
// after StartDrain. Wired in NewServer so every route inherits the
// behaviour without having to opt in. /api/restart and similar
// admin endpoints are NOT exempted — they should already have been
// initiated before StartDrain fired, and re-issuing during the drain
// window is exactly the case where the 503 helps the client retry on
// the new process.
func (s *Server) DrainMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Server restarting; retry in a few seconds"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	s.doShutdown()
}

// --- Gzip compression middleware ---

const gzipMinSize = 1024 // Only compress responses > 1KB

// CompressionMiddleware applies gzip compression to responses over 1KB.
func CompressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip if client doesn't accept gzip
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// Skip for WebSocket upgrade and streaming endpoints
		if r.Header.Get("Upgrade") != "" || shouldSkipCompression(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// The body this handler is about to produce depends on Accept-Encoding,
		// and T4 made ?v= assets `public, immutable` with ETags — so a SHARED
		// cache may now store one. Without Vary it would serve the gzipped copy
		// to the next client that negotiated identity, which cannot decode it.
		// Add, not Set: CORS and auth middlewares are free to name their own
		// fields, and a second Vary line is as valid as a longer one (R5/F5).
		w.Header().Add("Vary", "Accept-Encoding")

		gz := &gzipResponseWriter{
			ResponseWriter: w,
			minSize:        gzipMinSize,
		}
		// A handler that panics before anything reached the wire must leave
		// the response uncommitted. Close would commit it on the way out —
		// the 200 flushStatus defaults to, plus whatever half-built body sat
		// in the buffer — and RecoveryMiddleware, further out, would then
		// find headers sent and skip its 500: every browser asks for gzip,
		// so a panicking API call reached the dashboard as an empty 200.
		// Nothing is pooled before the headers go out, so there is nothing
		// to release either. Once they have, Close runs as before.
		completed := false
		defer func() {
			if !completed && !gz.headerSent {
				return
			}
			gz.Close()
		}()

		next.ServeHTTP(gz, r)
		completed = true
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer     *gzip.Writer
	buf        []byte
	minSize    int
	statusCode int
	headerSent bool
	// plain is set when an explicit Flush() arrived before the gzip
	// threshold: the response is committed to uncompressed output and all
	// further writes pass straight through. Without this, a handler that
	// writes a small payload and Flushes to guarantee delivery (e.g.
	// /api/update/apply flushing its success JSON before restarting the
	// process) would have its bytes stuck in buf until handler return.
	plain bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.statusCode = code
	if g.headerSent {
		return
	}
	// A 206's Content-Range describes an exact byte span of the identity
	// encoding. Gzipping the body would change its length without updating
	// Content-Range and pair Content-Encoding: gzip with a range the client
	// asked for against the uncompressed resource — malformed on the wire
	// (R16). Commit it plain immediately, the same way a body-less response
	// (e.g. 304) already reaches the client uncompressed by never touching
	// the gzip threshold.
	if code == http.StatusPartialContent {
		g.commitPlain()
		return
	}
	// Don't send headers yet — wait until we know if we need gzip
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if g.writer != nil {
		// Already gzipping
		return g.writer.Write(b)
	}
	if g.plain {
		// Committed to uncompressed output by an early Flush.
		return g.ResponseWriter.Write(b)
	}

	g.buf = append(g.buf, b...)

	if len(g.buf) >= g.minSize {
		if g.skipCompression() {
			g.commitPlain()
			return len(b), nil
		}
		// Buffer is big enough, start gzipping
		if err := g.startGzip(); err != nil {
			return 0, err
		}
		return len(b), nil
	}

	return len(b), nil
}

// gzipWriterPool recycles the deflate state a *gzip.Writer carries — a window,
// a hash head table and a hash prev table, measured at 1,080,105 B across 15
// allocations per fresh writer against 4,099 B across 1 for a recycled one.
// Every writer is Reset onto its response before use and returned on Close.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

func (g *gzipResponseWriter) startGzip() error {
	g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	g.ResponseWriter.Header().Del("Content-Length") // Length changes with compression
	g.flushStatus()
	g.headerSent = true

	gw := gzipWriterPool.Get().(*gzip.Writer)
	gw.Reset(g.ResponseWriter)
	g.writer = gw

	var err error
	if len(g.buf) > 0 {
		_, err = g.writer.Write(g.buf)
		g.buf = nil
	}
	return err
}

// skipCompression reports whether the response committed so far must go out
// uncompressed. Checked at the gzip threshold rather than up front, because the
// handler sets Content-Type while it writes.
//
//   - image/* and video/* are already compressed: gzip buys nothing and costs a
//     full CPU pass per response, which on a dashboard is one JPEG per job card
//     (sweep T2-18).
//   - a Content-Encoding the handler set itself must not be wrapped in a second
//     one; the client would decode gzip and find an encoded body underneath.
func (g *gzipResponseWriter) skipCompression() bool {
	h := g.ResponseWriter.Header()
	if h.Get("Content-Encoding") != "" {
		return true
	}
	ct := h.Get("Content-Type")
	return strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/")
}

func (g *gzipResponseWriter) flushStatus() {
	if g.statusCode == 0 {
		g.statusCode = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.statusCode)
}

func (g *gzipResponseWriter) Close() {
	if g.writer != nil {
		g.writer.Close()
		// Reset onto io.Discard before parking it: a pooled writer must not
		// keep this response's ResponseWriter alive, and g.writer is nilled so
		// a stray Flush after Close cannot touch a writer someone else owns.
		g.writer.Reset(io.Discard)
		gzipWriterPool.Put(g.writer)
		g.writer = nil
		return
	}

	// Buffer never reached threshold — send uncompressed
	g.commitPlain()
}

// commitPlain commits the response to uncompressed output: sends headers if
// still pending and drains any buffered bytes to the underlying writer.
// Idempotent.
func (g *gzipResponseWriter) commitPlain() {
	g.plain = true
	if !g.headerSent {
		g.flushStatus()
		g.headerSent = true
	}
	if len(g.buf) > 0 {
		g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
}

// Flush implements http.Flusher for streaming compatibility. A Flush that
// arrives before the gzip threshold commits the response to uncompressed
// output first — the caller is explicitly asking for the bytes written so
// far to reach the client NOW (e.g. /api/update/apply flushing its success
// JSON before triggering a process restart), and leaving them in the
// pre-threshold buffer would defeat exactly that.
func (g *gzipResponseWriter) Flush() {
	if g.writer != nil {
		g.writer.Flush()
	} else {
		g.commitPlain()
	}
	// Through the controller, not a type assertion: the writer underneath is
	// RecoveryMiddleware's, and an assertion only sees what that wrapper
	// itself implements.
	_ = http.NewResponseController(g.ResponseWriter).Flush()
}

// Unwrap allows http.ResponseController to access the underlying ResponseWriter.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

// Push implements http.Pusher if the underlying writer supports it.
func (g *gzipResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := g.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Hijack implements http.Hijacker for WebSocket support.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// withSessionCookie returns r carrying token as its moombox_session cookie.
// The client-token fallback mints a session and sets it on the RESPONSE, but
// the handler reads the REQUEST's cookie: there the stale or missing session
// still stood, so a logout invalidated the old token and left the new one
// alive for its whole TTL, and set-password answered 401 to a remote client
// the middleware had just authenticated. A copy, not an edit of r's headers.
func withSessionCookie(r *http.Request, token string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.Header = r.Header.Clone()
	r2.Header.Del("Cookie")
	for _, c := range r.Cookies() {
		if c.Name != "moombox_session" {
			r2.AddCookie(c)
		}
	}
	r2.AddCookie(&http.Cookie{Name: "moombox_session", Value: token})
	return r2
}

// recoveryWriter tracks whether headers have been sent so the recovery
// middleware can avoid writing a 500 response after a partial write.
type recoveryWriter struct {
	http.ResponseWriter
	headersSent bool
}

func (rw *recoveryWriter) WriteHeader(code int) {
	rw.headersSent = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recoveryWriter) Write(b []byte) (int, error) {
	rw.headersSent = true
	return rw.ResponseWriter.Write(b)
}

// Flush passes a handler's Flush through to the connection. Without it the
// whole chain swallowed every Flush: this wrapper sits outside the gzip one,
// whose Flush asserted http.Flusher on it and found nothing, so the handlers
// that answer before a blocking re-check (POST /api/cookies/import, the
// setup wizard's finish) held their response until the re-check ended —
// seconds, up to 45 — in production, while tests built on a bare router saw
// it arrive at once.
func (rw *recoveryWriter) Flush() {
	rw.headersSent = true
	_ = http.NewResponseController(rw.ResponseWriter).Flush()
}

func (rw *recoveryWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// panicStackFrames bounds the stack RecoveryMiddleware logs with a handler
// panic. The frames an operator needs — the function that panicked and the
// handler that called it — are the innermost ones; the outermost are the
// middleware chain and net/http's connection loop, the same for every request.
const panicStackFrames = 32

// panicStack renders the stack of the goroutine that is recovering a panic as
// ONE line, innermost frame first — "function (file:line)" per frame, joined
// by " < ", starting below the runtime's own panic machinery — and at most
// panicStackFrames frames of it. Call it from the deferred function that
// recovered: that is where the panicking frames are still on the stack.
//
// Built from program counters rather than debug.Stack. One line, because the
// same line reaches the ring buffer, every dashboard and the TUI log panel,
// none of which escape a newline the way the file handler does. And no
// argument values: debug.Stack prints each frame's raw argument words, the
// one part of a trace that comes from the request rather than from the code.
func panicStack() string {
	pcs := make([]uintptr, 128)
	n := runtime.Callers(2, pcs) // from the deferred function down
	frames := runtime.CallersFrames(pcs[:n])
	var all []runtime.Frame
	start := 0
	for {
		f, more := frames.Next()
		all = append(all, f)
		if f.Function == "runtime.gopanic" {
			start = len(all) // what panicked is below gopanic, not above it
		}
		if !more {
			break
		}
	}
	all = all[start:]

	var sb strings.Builder
	for i, f := range all {
		if i == panicStackFrames {
			atLeast := ""
			if n == len(pcs) {
				atLeast = "at least " // the capture itself was cut short
			}
			fmt.Fprintf(&sb, " < … %s%d more", atLeast, len(all)-i)
			break
		}
		if i > 0 {
			sb.WriteString(" < ")
		}
		file := f.File
		if slash := strings.LastIndexByte(file, '/'); slash >= 0 {
			file = file[slash+1:]
		}
		fmt.Fprintf(&sb, "%s (%s:%d)", f.Function, file, f.Line)
	}
	return sb.String()
}

// RecoveryMiddleware catches panics, logs them with the stack that raised
// them, and returns 500.
func RecoveryMiddleware(logger interface {
	Error(msg string, args ...any)
}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &recoveryWriter{ResponseWriter: w}
			defer func() {
				if rvr := recover(); rvr != nil {
					// Include the chi request ID so panic logs correlate with
					// any other log lines emitted during this request handling
					// (audit reports/web.md S-22). method+remoteAddr added per
					// audit Q-25 to make panic reports actionable without
					// needing the user to reproduce. The stack is what locates
					// the bug: without it a panic reported from the field said
					// what went wrong and never where (W24-15). Path, not URL —
					// the query string can carry a token.
					logger.Error("panic recovered in HTTP handler",
						"panic", rvr,
						"method", r.Method,
						"path", r.URL.Path,
						"remoteAddr", r.RemoteAddr,
						"reqID", chimiddleware.GetReqID(r.Context()),
						"stack", panicStack(),
					)
					if !rw.headersSent {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte(`{"error":"Internal server error"}`))
					}
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// outermostRecovery is the server's own outermost handler (serverHandler),
// for a panic every recover inside it misses: chi's RequestID and
// DrainMiddleware run ahead of RecoveryMiddleware by design, and
// interceptUpgrades' own gates (ipAllowedByNetworkAccess,
// externalHostRefused) ahead of the router and of HandleUpgrade's recover.
// Such a panic reached net/http's own recover, which writes its report to
// the server's ErrorLog — discarded — so it was logged nowhere, and the
// client saw its connection dropped.
//
// Logged here as RecoveryMiddleware logs one: the panic value, the method,
// the path (never the query, which can carry a token), the peer and the
// bounded, argument-free panicStack. No request ID: RequestID, inside, has
// not necessarily run. The client gets the same 500 when nothing has
// reached it yet; when something has, the panic goes back to net/http as
// http.ErrAbortHandler, which drops the connection without a report of its
// own. A handler that panics with http.ErrAbortHandler itself is aborting on
// purpose, and passes through untouched and unlogged.
func outermostRecovery(logger interface {
	Error(msg string, args ...any)
}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoveryWriter{ResponseWriter: w}
		defer func() {
			rvr := recover()
			if rvr == nil {
				return
			}
			if rvr == http.ErrAbortHandler {
				panic(rvr)
			}
			logger.Error("panic recovered outside the HTTP middleware chain",
				"panic", rvr,
				"method", r.Method,
				"path", r.URL.Path,
				"remoteAddr", r.RemoteAddr,
				"stack", panicStack(),
			)
			if rw.headersSent {
				panic(http.ErrAbortHandler)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"Internal server error"}`))
		}()
		next.ServeHTTP(rw, r)
	})
}

// OpenPathCommand builds the command that hands `target` — a URL or a
// directory — to the desktop shell: explorer.exe on Windows, xdg-open on the
// supported Linux targets (the freedesktop standard, and what opens a file
// manager for a directory there), `open` on macOS. The caller starts it
// (StartDetached, below); nothing here spawns.
//
// Exported so every open-path site in the program shares this ONE switch —
// internal/web/routes' open-folder handler and cmd/moombox's `O F` chord as
// well as openBrowserURL. They did not: the route named the Windows file
// manager inline with no switch at all, so on a Linux desktop the dashboard's
// Open Folder button appeared (the host is loopback), found no explorer, and
// 500'd into a client that never read response.ok (WEB-6).
//
// macOS is not a supported target. The arm exists because the TUI chord folded
// onto this helper had one, and folding must not delete working code.
func OpenPathCommand(target string) *exec.Cmd {
	return openPathCommandFor(runtime.GOOS, target)
}

// openPathCommandFor is OpenPathCommand with the platform injected, so every
// shape is assertable from either host: the Windows spawn this change must
// leave byte-identical cannot be checked from a Linux CI runner otherwise, and
// the Linux spawn it adds cannot be checked from the Windows desktop this
// project is developed on.
//
// The program is a variable rather than one exec.Command call per branch,
// because a second literal is exactly how the copies of this switch drifted
// apart — TestOpenBrowserURLUsesTheSharedCommand counts both the literals and
// the spawn primitives, and wants one of each in the whole file.
func openPathCommandFor(goos, target string) *exec.Cmd {
	program := "xdg-open"
	switch goos {
	case "windows":
		program = "explorer.exe"
	case "darwin":
		program = "open"
	}
	cmd := exec.Command(program, target)
	// Windows only: Go leaves an unquoted '=' for explorer's legacy parser to
	// split, so a directory path containing one opens nothing (W R-3). The
	// argv above is unchanged — Windows ignores it once CmdLine is set.
	if goos == "windows" {
		forceQuoteCmdLine(cmd, program, target)
	}
	return cmd
}

// StartDetached starts cmd and hands the child back to the OS. The caller never
// waits: every site that uses this opens a desktop window that outlives the
// request.
//
// The two platforms need OPPOSITE things, which is why this is a helper and not
// a Start() at each call site:
//
//   - Windows: Process.Release() returns the process HANDLE to the kernel.
//     Without it one handle leaks per call for the life of Moombox (audit
//     reports/web.md Q-6). There is nothing to reap — Windows has no zombies —
//     and a Wait here would park a goroutine for as long as the window is open,
//     which on a session where explorer.exe IS the shell is forever.
//   - Everywhere else: Release() closes the pidfd and never calls wait4, so a
//     child nobody Waits for stays a ZOMBIE in the process table until the
//     parent exits, and Moombox runs for weeks. The Wait goes in a goroutine so
//     the HTTP handler answers immediately.
//
// Returns Start's error only; the detach itself is best-effort.
func StartDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	detachStarted(runtime.GOOS, cmdChild{cmd})
	return nil
}

// startedChild is the half of a started *exec.Cmd that detachStarted uses. An
// interface because the two arms cannot otherwise be exercised: a real child
// here is a file-manager window on the developer's desktop.
type startedChild interface {
	Wait() error
	Release() error
}

// cmdChild adapts *exec.Cmd — Wait is the Cmd's, Release is its Process's.
type cmdChild struct{ cmd *exec.Cmd }

func (c cmdChild) Wait() error { return c.cmd.Wait() }

func (c cmdChild) Release() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Release()
}

// detachStarted is StartDetached's platform decision with the child injected.
// See StartDetached for why the arms differ.
func detachStarted(goos string, child startedChild) {
	if goos == "windows" {
		_ = child.Release()
		return
	}
	go func() {
		// The project's inline-recover rule (CLAUDE.md). There is no logger on
		// this path — every caller is a fire-and-forget spawn with no Server in
		// hand — and waiting on an already-started child does not panic in
		// practice, so this is a backstop against taking the process down, not
		// a report.
		defer func() {
			if r := recover(); r != nil {
				_ = r
			}
		}()
		_ = child.Wait()
	}()
}

// openBrowserURL opens the default browser to the given URL. Failure is silent
// best-effort: the dashboard is already listening, and the URL is in the log.
// StartDetached owns what happens to the child afterwards, per platform.
func openBrowserURL(url string) {
	cmd := OpenPathCommand(url)
	_ = StartDetached(cmd)
}

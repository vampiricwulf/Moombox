// Package web provides the HTTP server and WebSocket handler for Moombox.
package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io/fs"
	"io"
	"log"
	"sync/atomic"
	"net"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"sync"
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
	configStore   *config.Store // Authoritative cfg + mutex (DECISIONS #8)
	cfg           *config.MoomboxConfig
	router        chi.Router
	server        *http.Server
	ws            *WebSocketHub
	auth          *AuthService
	shutdownOnce  sync.Once        // Ensures shutdown logic runs only once
	internalToken string           // Random secret for same-process CSRF bypass
	commit        string           // Build commit hash for cache busting (e.g. "abc1234")
	loginHTML     []byte           // Cached login.html for inline serving (matches TS serveLoginPage)
	wsHandler     http.HandlerFunc // WebSocket upgrade handler (intercepts upgrades on any path)
	OpenBrowser   bool             // Open browser to dashboard URL on start (matches TS openBrowser option)
	ActualPort    int              // Actual bound port after Start (may differ from cfg if probed)
	draining      atomic.Bool      // Set by StartDrain to make new requests 503 (audit cmd-moombox C-main:165-166)

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
	r.Use(CORSMiddleware(store))
	r.Use(SecurityHeaders)
	r.Use(CSRFMiddleware(store, token))
	r.Use(IPGateMiddleware(store))
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

// SetAuth sets the auth service for authentication middleware.
func (s *Server) SetAuth(auth *AuthService) {
	s.auth = auth
}

// AuthMiddleware checks authentication for external connections.
// Loopback and LAN clients skip auth. External clients need a valid session
// when a password is configured.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ExtractIP(r)

		// Loopback and private IPs skip auth
		if isLoopback(ip) || isPrivateIP(ip) {
			next.ServeHTTP(w, r)
			return
		}

		// No auth required if not configured
		var networkAccess, passwordHash string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			networkAccess = c.Network.NetworkAccess
			passwordHash = c.Network.PasswordHash
		})
		if !IsAuthRequired(networkAccess, passwordHash) {
			next.ServeHTTP(w, r)
			return
		}

		// Unauthenticated paths (login page, auth endpoints, POT read-only, favicon)
		p := r.URL.Path
		if p == "/api/auth/login" ||
			p == "/api/auth/status" ||
			p == "/ping" || p == "/minter_cache" ||
			p == "/favicon.svg" || p == "/login.html" {
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
					next.ServeHTTP(w, r)
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

	// Read index.html once for SPA fallback, with cache-busted asset URLs
	indexHTML, _ := fs.ReadFile(staticFS, "index.html")
	if indexHTML != nil && s.commit != "" {
		suffix := []byte("?v=" + s.commit)
		indexHTML = bytes.ReplaceAll(indexHTML, []byte(`"/moombox.css"`), []byte(`"/moombox.css`+string(suffix)+`"`))
		indexHTML = bytes.ReplaceAll(indexHTML, []byte(`"/app.js"`), []byte(`"/app.js`+string(suffix)+`"`))
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

		// Check if the file exists in the embedded FS
		if f, err := staticFS.Open(urlPath); err == nil {
			f.Close()
			// Set cache headers for static assets
			ext := path.Ext(urlPath)
			switch ext {
			case ".png", ".jpg", ".svg", ".ico":
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			case ".css", ".js":
				w.Header().Set("Cache-Control", "no-cache")
			default:
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback: serve index.html for non-file routes
		if indexHTML != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(indexHTML)
			return
		}

		http.NotFound(w, r)
	})
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

	// Wrap router to intercept WebSocket upgrades on any path (matches TS noServer mode)
	var handler http.Handler = s.router
	if s.wsHandler != nil {
		wsHandler := s.wsHandler
		router := s.router
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				wsHandler(w, r)
				return
			}
			router.ServeHTTP(w, r)
		})
	}

	s.server = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second, // Protects against slowloris; clears deadline after headers are read
		WriteTimeout:      0,                // Disable for WebSocket and video streaming
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // Suppress HTTP server errors from stdout/stderr (routed through app logger via middleware)
	}

	// Configure TLS if enabled
	scheme := "http"
	var tlsConfig *tls.Config
	if s.cfg.Network.HTTPSEnabled {
		certPath := s.cfg.Network.TLSCertPath
		if certPath == "" {
			certPath = "./moombox.crt"
		}
		keyPath := s.cfg.Network.TLSKeyPath
		if keyPath == "" {
			keyPath = "./moombox.key"
		}

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

	// Wrap listener with TLS if enabled
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}

	// Log the actual URL (matches TS: "Web dashboard available at ...")
	actualPort := ln.Addr().(*net.TCPAddr).Port
	s.ActualPort = actualPort
	url := fmt.Sprintf("%s://localhost:%d", scheme, actualPort)
	s.logger.Info(fmt.Sprintf("[Moombox] Web dashboard available at %s", url))
	if s.cfg.Network.NetworkAccess == "lan" || s.cfg.Network.NetworkAccess == "external" {
		s.logger.Info(fmt.Sprintf("[WebServer] LAN access enabled (listening on %s)", host))
	}
	if (s.cfg.Network.NetworkAccess == "external" || s.cfg.Network.NetworkAccess == "public") && s.cfg.Network.PasswordHash != "" && !s.cfg.Network.HTTPSEnabled {
		s.logger.Warn("[WebServer] External access with authentication over plain HTTP — session cookies are not encrypted. Consider setting https_enabled = true or using a reverse proxy with HTTPS.")
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
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.server.Shutdown(ctx)
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
// behaviour without having to opt in. Loopback /api/restart and similar
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

		gz := &gzipResponseWriter{
			ResponseWriter: w,
			minSize:        gzipMinSize,
		}
		defer gz.Close()

		next.ServeHTTP(gz, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer     *gzip.Writer
	buf        []byte
	minSize    int
	statusCode int
	headerSent bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.statusCode = code
	if g.headerSent {
		return
	}
	// Don't send headers yet — wait until we know if we need gzip
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if g.writer != nil {
		// Already gzipping
		return g.writer.Write(b)
	}

	g.buf = append(g.buf, b...)

	if len(g.buf) >= g.minSize {
		// Buffer is big enough, start gzipping
		if err := g.startGzip(); err != nil {
			return 0, err
		}
		return len(b), nil
	}

	return len(b), nil
}

func (g *gzipResponseWriter) startGzip() error {
	g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	g.ResponseWriter.Header().Del("Content-Length") // Length changes with compression
	g.flushStatus()
	g.headerSent = true

	var err error
	g.writer, err = gzip.NewWriterLevel(g.ResponseWriter, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	if len(g.buf) > 0 {
		_, err = g.writer.Write(g.buf)
		g.buf = nil
	}
	return err
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
		return
	}

	// Buffer never reached threshold — send uncompressed
	if !g.headerSent {
		g.flushStatus()
		g.headerSent = true
	}
	if len(g.buf) > 0 {
		g.ResponseWriter.Write(g.buf)
	}
}

// Flush implements http.Flusher for streaming compatibility.
func (g *gzipResponseWriter) Flush() {
	if g.writer != nil {
		g.writer.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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

func (rw *recoveryWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// RecoveryMiddleware catches panics and returns 500.
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
					// needing the user to reproduce.
					logger.Error("panic recovered in HTTP handler",
						"panic", rvr,
						"method", r.Method,
						"path", r.URL.Path,
						"remoteAddr", r.RemoteAddr,
						"reqID", chimiddleware.GetReqID(r.Context()),
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

// openBrowserURL opens the default browser to the given URL.
// Windows-only: uses explorer.exe to launch the URL.
//
// Process.Release() returns the OS handle to the kernel so we don't
// leak one process handle per Moombox lifetime. Symmetric with the
// open-folder handler's Q-6 fix (audit reports/web.md).
func openBrowserURL(url string) {
	cmd := exec.Command("explorer.exe", url)
	if err := cmd.Start(); err == nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}


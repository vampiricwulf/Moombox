package web

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// resolveTLSPaths applies the default certificate/key locations. Single
// source for the defaults — Start's HTTPS branch and the https→http
// redirect's load-only path must agree on where the cert pair lives.
func resolveTLSPaths(cfg *config.MoomboxConfig) (certPath, keyPath string) {
	certPath = cfg.Network.TLSCertPath
	if certPath == "" {
		certPath = "./moombox.crt"
	}
	keyPath = cfg.Network.TLSKeyPath
	if keyPath == "" {
		keyPath = "./moombox.key"
	}
	return certPath, keyPath
}

// tlsHandshakeByte is the first byte of every TLS record-layer handshake
// (ContentType handshake = 22). Plain HTTP requests start with an ASCII
// method letter, so one peeked byte cleanly separates the two protocols on
// a single port.
const tlsHandshakeByte = 0x16

// schemePeekTimeout bounds how long a freshly-accepted connection may sit
// silent before the protocol sniffer gives up and closes it. Keeps idle
// port-scanner connections from pinning peek goroutines.
const schemePeekTimeout = 10 * time.Second

// peekedConn stitches the sniffed byte back in front of the connection's
// remaining stream.
type peekedConn struct {
	net.Conn
	reader io.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// muxedListener is one protocol branch of newSchemeMux: a net.Listener fed
// from a channel of pre-sniffed connections. Closing EITHER branch closes
// the shared underlying TCP listener (there is one real socket), which in
// turn unblocks both branches' Accept — so http.Server.Shutdown on the main
// server tears the whole mux down.
type muxedListener struct {
	conns     chan net.Conn
	addr      net.Addr
	closeReal func()
	done      chan struct{}
}

func (l *muxedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		// Drain race: a sniffed connection may have been queued between the
		// real listener dying and this Accept observing done.
		select {
		case c := <-l.conns:
			return c, nil
		default:
		}
		return nil, net.ErrClosed
	}
}

func (l *muxedListener) Close() error {
	l.closeReal()
	return nil
}

func (l *muxedListener) Addr() net.Addr { return l.addr }

// newSchemeMux splits real's accepted connections by protocol: connections
// whose first byte is a TLS handshake go to the first returned listener,
// everything else to the second. The sniff happens on a per-connection
// goroutine so a slow client can't stall the accept loop.
func newSchemeMux(real net.Listener, logger interface {
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}) (tlsLn, plainLn net.Listener) {
	done := make(chan struct{})
	var closeOnce sync.Once
	closeReal := func() { closeOnce.Do(func() { real.Close() }) }

	tlsML := &muxedListener{conns: make(chan net.Conn), addr: real.Addr(), closeReal: closeReal, done: done}
	plainML := &muxedListener{conns: make(chan net.Conn), addr: real.Addr(), closeReal: closeReal, done: done}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("scheme mux accept loop panic", "panic", r)
			}
		}()
		defer close(done)
		for {
			conn, err := real.Accept()
			if err != nil {
				return // listener closed (shutdown) or fatal accept error
			}
			go func(conn net.Conn) {
				defer func() {
					if r := recover(); r != nil {
						conn.Close()
						logger.Error("scheme sniff panic", "panic", r)
					}
				}()
				conn.SetReadDeadline(time.Now().Add(schemePeekTimeout))
				var first [1]byte
				if _, err := io.ReadFull(conn, first[:]); err != nil {
					conn.Close()
					return
				}
				conn.SetReadDeadline(time.Time{})
				pc := &peekedConn{
					Conn:   conn,
					reader: io.MultiReader(bytes.NewReader(first[:]), conn),
				}
				target := plainML
				if first[0] == tlsHandshakeByte {
					target = tlsML
				}
				select {
				case target.conns <- pc:
				case <-done:
					pc.Close()
				}
			}(conn)
		}
	}()

	return tlsML, plainML
}

// schemeRedirectHandler redirects every request to the same host and path on
// targetScheme. 307 (temporary, method-preserving), NOT 301/308: browsers
// cache permanent redirects, and a user who later toggles https_enabled
// would be trapped in a cached cross-scheme redirect loop.
//
// listenPort is the port this service is actually bound to ("" to trust
// r.Host verbatim). It matters because the SAME socket serves both schemes:
// when the request arrived on the source scheme's default port, browsers
// omit the port from the Host header, and a bare cross-scheme redirect would
// send them to the TARGET scheme's default port — where nothing listens. A
// port-80 deployment redirecting http→https must say "https://host:80/",
// not "https://host/" (= port 443).
func schemeRedirectHandler(targetScheme, listenPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		host := r.Host
		if listenPort != "" {
			if _, _, err := net.SplitHostPort(host); err != nil {
				// Host carries no port — re-pin ours unless it IS the target
				// scheme's default. Strip IPv6 brackets before JoinHostPort
				// re-adds them.
				targetDefault := "80"
				if targetScheme == "https" {
					targetDefault = "443"
				}
				if listenPort != targetDefault {
					bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
					host = net.JoinHostPort(bare, listenPort)
				}
			}
		}
		// r.URL.RequestURI() (not r.RequestURI) keeps origin-form even for
		// proxy-style absolute-form request lines, which would otherwise
		// double the host.
		http.Redirect(w, r, targetScheme+"://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// listenerPort extracts the numeric port from a listener's address, or ""
// when it can't be determined (the redirect handler then trusts r.Host).
func listenerPort(ln net.Listener) string {
	if ln == nil || ln.Addr() == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return ""
	}
	return port
}

// loadRedirectTLSConfig LOADS (never generates) the configured certificate
// pair so the https→http redirect can terminate TLS when HTTPS is disabled.
// Typically these files exist from an earlier https_enabled run — exactly
// the case where stale https:// bookmarks need redirecting. Without a
// loadable certificate a TLS handshake cannot complete at all, so the
// caller skips the TLS branch entirely; generating a cert the user didn't
// ask for would be wrong here.
func loadRedirectTLSConfig(cfg *config.MoomboxConfig) *tls.Config {
	certPath, keyPath := resolveTLSPaths(cfg)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// serveSchemeRedirect runs a minimal HTTP server on ln whose only job is
// issuing cross-scheme redirects. It exits when the shared underlying
// listener closes (main-server shutdown).
func (s *Server) serveSchemeRedirect(ln net.Listener, targetScheme string) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("scheme-redirect server panic", "panic", r)
		}
	}()
	srv := &http.Server{
		Handler:           schemeRedirectHandler(targetScheme, listenerPort(ln)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		s.logger.Debug("scheme-redirect listener closed", "err", err)
	}
}

// Package httpx provides a single source of truth for the *http.Client
// shapes Moombox uses across packages. Callers pass in a timeout and
// get back a client backed by the shared keep-alive-tuned transport,
// so per-package tuning drift can't accumulate.
//
// Audit cross-cutting (bgutils DEDUP-2 + cipher DU5 + utils HTTP
// transport tuning).
package httpx

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// defaultTransport is the shared *http.Transport used by Client(). Tuning matches utils' previous utilsHTTPClient:
//
//   - ForceAttemptHTTP2: true — match upstream YouTube/Twitch CDN
//     expectations; HTTP/2 multiplexing reduces handshake cost on
//     parallel segment fetches.
//   - MaxIdleConnsPerHost: 8 — Go's default of 2 is too low; bumping
//     to 8 lets 4-6 concurrent fetches reuse keep-alive connections
//     instead of triggering fresh handshakes.
//   - IdleConnTimeout: 90s — keep idle conns warm long enough that
//     periodic monitor/refresh ticks reuse them.
//   - HTTP2 SendPingTimeout 30s, PingTimeout 15s — an HTTP/2 connection
//     that has delivered no frame for 30 s is pinged, and closed when the
//     ping goes unanswered for 15 s. Without the health check a
//     connection whose peer vanished without a RST (a NAT or firewall
//     that dropped the flow, a VPN flap, a Docker bridge hiccup) stays in
//     the pool: a request cancelled on it only resets its stream, so
//     every later request to that host multiplexes onto the same dead
//     connection until the kernel gives up retransmitting (about 15
//     minutes on Linux defaults). An idle pooled connection is pinged
//     too, until IdleConnTimeout closes it.
//   - DialContext: a 30 s connect timeout, http.DefaultTransport's. The
//     zero-value dialer this replaced had none, so a connect to a host
//     that drops SYNs waited out the OS's own limit. KeepAlive is left
//     unset on purpose: that keeps the dialer's default 15 s TCP
//     keep-alive, the value in use before; DefaultTransport's 30 s would
//     have halved the probe rate.
//
// Engine downloads use a custom-built transport with a fixed, generously
// sized MaxIdleConnsPerHost (engineMaxIdleConnsPerHost = 64) — see the
// engine package for that. All other packages should use this default via
// Client().
//
// The transport is package-private so callers can't mutate it; if a
// package needs a different shape, build a fresh Transport via
// NewTransport(opts) and pass it to ClientWithTransport.
var defaultTransport = newDefaultTransport()

// The shared transport's connection-health bounds; see defaultTransport.
const (
	dialTimeout          = 30 * time.Second
	http2SendPingTimeout = 30 * time.Second
	http2PingTimeout     = 15 * time.Second
)

func newDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: http2SendPingTimeout,
			PingTimeout:     http2PingTimeout,
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// Client returns a new *http.Client with the given timeout and the
// shared default transport. timeout=0 means "no client-level timeout"
// — the caller is expected to use ctx.WithTimeout for per-request
// cancellation.
//
// No CookieJar is installed, and callers rely on that: internal/youtube's
// liveness probes detect a credential-stripping redirect by checking whether
// the answering request still carried the Cookie header they set, which a jar
// would re-add from its own scope rules. Adding one here would silently
// invalidate that guard — see the comment on internal/utils.utilsHTTPClient
// and TestUtilsHTTPClientCarriesNoCookieJar, which pins it for the client
// those probes actually use.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: defaultTransport}
}

// ClientWithTransport returns a new *http.Client with the given
// timeout and a caller-supplied transport. Useful when a package
// needs MaxIdleConnsPerHost-style tuning beyond the default.
func ClientWithTransport(timeout time.Duration, transport http.RoundTripper) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}

// TransportOptions configure a NewTransport build. Zero values fall
// back to the defaults baked into newDefaultTransport().
type TransportOptions struct {
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	ExpectContinueTimeout time.Duration
	DisableHTTP2          bool
	// HTTP2SendPingTimeout and HTTP2PingTimeout override the HTTP/2 health
	// check's two bounds, and DialTimeout the connect timeout. They exist so
	// a test can reach them in milliseconds; production callers leave them
	// zero.
	HTTP2SendPingTimeout time.Duration
	HTTP2PingTimeout     time.Duration
	DialTimeout          time.Duration
}

// NewTransport builds a fresh *http.Transport from the given options.
// Callers that need fine-grained tuning (e.g. engine's fixed, generous
// MaxIdleConnsPerHost sizing for high-concurrency segment downloads)
// use this; everyone else should use Client() with the shared default.
func NewTransport(opts TransportOptions) *http.Transport {
	t := newDefaultTransport()
	if opts.MaxIdleConns > 0 {
		t.MaxIdleConns = opts.MaxIdleConns
	}
	if opts.MaxIdleConnsPerHost > 0 {
		t.MaxIdleConnsPerHost = opts.MaxIdleConnsPerHost
	}
	if opts.IdleConnTimeout > 0 {
		t.IdleConnTimeout = opts.IdleConnTimeout
	}
	if opts.TLSHandshakeTimeout > 0 {
		t.TLSHandshakeTimeout = opts.TLSHandshakeTimeout
	}
	if opts.ExpectContinueTimeout > 0 {
		t.ExpectContinueTimeout = opts.ExpectContinueTimeout
	}
	if opts.HTTP2SendPingTimeout > 0 {
		t.HTTP2.SendPingTimeout = opts.HTTP2SendPingTimeout
	}
	if opts.HTTP2PingTimeout > 0 {
		t.HTTP2.PingTimeout = opts.HTTP2PingTimeout
	}
	if opts.DialTimeout > 0 {
		t.DialContext = (&net.Dialer{Timeout: opts.DialTimeout}).DialContext
	}
	if opts.DisableHTTP2 {
		// Both are set. ForceAttemptHTTP2=false alone disables HTTP/2 only
		// while the transport carries a custom dialer or TLS config (it
		// does now; it did not always). A non-nil empty TLSNextProto map
		// is the documented opt-out, and holds either way.
		t.ForceAttemptHTTP2 = false
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return t
}

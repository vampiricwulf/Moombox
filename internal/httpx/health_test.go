package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every transport this package hands out carries the HTTP/2 health check and
// a connect timeout: the shared one behind Client() and each NewTransport
// build, the engine's among them.
//
// Mutants: the HTTP2 or DialContext line dropped from newDefaultTransport;
// newDialer building a dialer with no Timeout, or with a KeepAlive of its own
// (30 s would halve the keep-alive probe rate in use before).
func TestTransportsCarryTheHealthCheckAndDialTimeout(t *testing.T) {
	if dialTimeout != 30*time.Second || http2SendPingTimeout != 30*time.Second || http2PingTimeout != 15*time.Second {
		t.Errorf("dial %v, send-ping %v, ping %v; want 30 s, 30 s, 15 s", dialTimeout, http2SendPingTimeout, http2PingTimeout)
	}
	if d := newDialer(dialTimeout); d.Timeout != 30*time.Second || d.KeepAlive != 0 {
		t.Errorf("dialer: Timeout %v, KeepAlive %v; want 30 s and the default keep-alive (0)", d.Timeout, d.KeepAlive)
	}
	shared, ok := Client(0).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Client().Transport is %T, want *http.Transport", Client(0).Transport)
	}
	for name, tr := range map[string]*http.Transport{
		"Client()":                   shared,
		"NewTransport()":             NewTransport(TransportOptions{}),
		"NewTransport(pool size)":    NewTransport(TransportOptions{MaxIdleConnsPerHost: 64}),
		"NewTransport(DisableHTTP2)": NewTransport(TransportOptions{DisableHTTP2: true}),
	} {
		if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != http2SendPingTimeout || tr.HTTP2.PingTimeout != http2PingTimeout {
			t.Errorf("%s: HTTP2 = %+v, want the health check's two timeouts", name, tr.HTTP2)
		}
		if tr.DialContext == nil {
			t.Errorf("%s: no DialContext, so no connect timeout", name)
		}
		if name != "NewTransport(DisableHTTP2)" && !tr.ForceAttemptHTTP2 {
			t.Errorf("%s: ForceAttemptHTTP2 is off; a custom dialer needs it for HTTP/2", name)
		}
	}
	// Two builds never share the config an override edits.
	if a, b := NewTransport(TransportOptions{}), NewTransport(TransportOptions{}); a.HTTP2 == b.HTTP2 || a.HTTP2 == shared.HTTP2 {
		t.Error("transports share one *http.HTTP2Config")
	}
	tr := NewTransport(TransportOptions{HTTP2SendPingTimeout: time.Second, HTTP2PingTimeout: 2 * time.Second, DialTimeout: time.Second})
	if tr.HTTP2.SendPingTimeout != time.Second || tr.HTTP2.PingTimeout != 2*time.Second {
		t.Errorf("overrides not applied: %+v", tr.HTTP2)
	}
	if shared.HTTP2.SendPingTimeout != http2SendPingTimeout || shared.HTTP2.PingTimeout != http2PingTimeout {
		t.Error("an override reached the shared transport")
	}
}

// blackHoleProxy is a TCP relay in front of a server. freeze makes every
// connection open at that moment a black hole: bytes from either side are
// swallowed and nothing is closed or reset, which is what a NAT or firewall
// that dropped the flow looks like. Connections made afterwards work.
type blackHoleProxy struct {
	ln       net.Listener
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
	frozen   []*atomic.Bool
}

func newBlackHoleProxy(t *testing.T, backend string) *blackHoleProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackHoleProxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", backend)
			if err != nil {
				c.Close()
				continue
			}
			frozen := new(atomic.Bool)
			p.mu.Lock()
			p.conns = append(p.conns, c, b)
			p.frozen = append(p.frozen, frozen)
			p.mu.Unlock()
			p.accepted.Add(1)
			go relay(b, c, frozen)
			go relay(c, b, frozen)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			c.Close()
		}
	})
	return p
}

// relay copies src to dst until src ends, dropping everything once frozen.
func relay(dst, src net.Conn, frozen *atomic.Bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !frozen.Load() {
				dst.Close()
			}
			return
		}
	}
}

func (p *blackHoleProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.frozen {
		f.Store(true)
	}
}

// An HTTP/2 connection that goes silent without a reset is closed by the
// health check, so the next request is made on a new connection. Without the
// check the dead connection stays in the pool and takes the next request
// with it: a request cancelled on it only resets its stream.
//
// Mutant: the HTTP2 config dropped from newDefaultTransport — the "with"
// row fails as the "without" row does.
func TestASilentlyDeadHTTP2ConnectionIsReplaced(t *testing.T) {
	for _, tc := range []struct {
		name        string
		healthCheck bool
	}{
		{"with the health check the next request gets a new connection", true},
		{"without it the next request waits on the dead one", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, r.Proto)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)
			proxy := newBlackHoleProxy(t, srv.Listener.Addr().String())

			// Long enough that a slow runner does not go a whole interval
			// silent, and miss a ping, while the connection is still good.
			const ping = 250 * time.Millisecond
			tr := NewTransport(TransportOptions{HTTP2SendPingTimeout: ping, HTTP2PingTimeout: ping})
			if !tc.healthCheck {
				tr.HTTP2 = nil
			}
			pool := x509.NewCertPool()
			pool.AddCert(srv.Certificate())
			tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			t.Cleanup(tr.CloseIdleConnections)
			client := ClientWithTransport(0, tr)
			url := "https://" + proxy.ln.Addr().String() + "/"

			get := func(timeout time.Duration) (string, error) {
				ctx, cancel := context.WithTimeout(t.Context(), timeout)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if err != nil {
					return "", err
				}
				resp, err := client.Do(req)
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				b, err := io.ReadAll(resp.Body)
				return string(b), err
			}

			if proto, err := get(10 * time.Second); err != nil || proto != "HTTP/2.0" {
				t.Fatalf("first request = %q, %v; want HTTP/2.0", proto, err)
			}
			if n := proxy.accepted.Load(); n != 1 {
				t.Fatalf("%d connections after the first request, want 1", n)
			}
			proxy.freeze()

			// This request is lost with the connection either way. It only
			// has to outlast the health check: one silent ping interval and
			// one unanswered ping.
			if _, err := get(8 * ping); err == nil {
				t.Fatal("a request on the black-holed connection succeeded")
			}

			if tc.healthCheck {
				proto, err := get(10 * time.Second)
				if err != nil || proto != "HTTP/2.0" {
					t.Fatalf("request after the dead connection = %q, %v; want it served on a new connection", proto, err)
				}
				if n := proxy.accepted.Load(); n != 2 {
					t.Errorf("%d connections, want 2: the dead one and its replacement", n)
				}
				return
			}
			if _, err := get(4 * ping); err == nil {
				t.Fatal("without the health check the next request succeeded: this row no longer shows what the check is for")
			}
			if n := proxy.accepted.Load(); n != 1 {
				t.Errorf("%d connections, want 1: the request went back onto the dead connection", n)
			}
		})
	}
}

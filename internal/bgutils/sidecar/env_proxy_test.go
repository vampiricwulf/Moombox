package sidecar

import (
	"bufio"
	"context"
	"net"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/http/httpproxy"
)

// Audit G22. The Node child's environment is Moombox's own with the proxy
// variables replaced by Go's own reading of them: every HTTPS_PROXY,
// HTTP_PROXY, NO_PROXY and NODE_USE_ENV_PROXY spelling taken out, and only
// the HTTPS proxy Go would use (its scheme filled in, as Go fills it),
// Go's NO_PROXY and NODE_USE_ENV_PROXY=1 put back. Node reads the raw
// variables otherwise than Go: it refuses to start on a scheme-less proxy,
// sends https through an HTTP_PROXY Go keeps for http://, and takes an empty
// lowercase name over a set uppercase one.
//
// Mutants: the raw variables passed through unfiltered (the HTTP_PROXY-only
// and empty-lowercase rows hand Node a proxy Go does not use, or the other
// way round); the proxy handed over as written, not normalised (the
// scheme-less row gives Node a URL it refuses to start on); the flag set
// with no proxy (the direct rows); a socks5 proxy handed over (Node refuses
// to start on it).
func TestChildEnvHandsNodeGosOwnProxy(t *testing.T) {
	base := []string{"PATH=/usr/bin", "NODE_EXTRA_CA_CERTS=/etc/corp-ca.pem"}
	for _, tc := range []struct {
		name     string
		parent   []string
		want     []string // proxy entries the child must get, in order, after base
		wantNote bool
	}{
		{
			name:   "scheme-less HTTPS_PROXY is http:// as Go reads it",
			parent: []string{"HTTPS_PROXY=proxy.corp.example:3128"},
			want:   []string{"HTTPS_PROXY=http://proxy.corp.example:3128", "NODE_USE_ENV_PROXY=1"},
		},
		{
			name:   "lowercase https_proxy alone, with NO_PROXY",
			parent: []string{"https_proxy=http://p.example:8080", "no_proxy=.corp.example,localhost"},
			want:   []string{"HTTPS_PROXY=http://p.example:8080", "NO_PROXY=.corp.example,localhost", "NODE_USE_ENV_PROXY=1"},
		},
		{
			name:   "HTTP_PROXY alone is direct for https, as in Go",
			parent: []string{"HTTP_PROXY=http://plain.example:3128", "http_proxy=http://plain.example:3128"},
		},
		{
			name:   "an empty https_proxy beside a set HTTPS_PROXY: Go takes the uppercase",
			parent: []string{"https_proxy=", "HTTPS_PROXY=http://p.example:8080"},
			want:   []string{"HTTPS_PROXY=http://p.example:8080", "NODE_USE_ENV_PROXY=1"},
		},
		{
			name:   "an inherited NODE_USE_ENV_PROXY is dropped with no proxy",
			parent: []string{"NODE_USE_ENV_PROXY=1", "NO_PROXY=*"},
		},
		{
			name:     "a socks5 proxy Node cannot use is not handed over",
			parent:   []string{"HTTPS_PROXY=socks5://s.example:1080", "NODE_USE_ENV_PROXY=1"},
			wantNote: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := append(slices.Clone(base), tc.parent...)
			proxy := proxyConfigOf(parent)
			got, note := childEnv(parent, &proxy)
			want := append(slices.Clone(base), tc.want...)
			if !slices.Equal(got, want) {
				t.Errorf("childEnv(%q) =\n  %q\nwant\n  %q", tc.parent, got, want)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q, want a note: %v", note, tc.wantNote)
			}
			if strings.Contains(note, "s.example") {
				t.Errorf("note %q names the proxy, which may carry credentials", note)
			}
		})
	}
}

// proxyConfigOf reads parent the way httpproxy.FromEnvironment reads the
// process's environment (uppercase first, an empty value as unset), so the
// table can state its environment as a list.
func proxyConfigOf(parent []string) httpproxy.Config {
	vals := map[string]string{}
	for _, kv := range parent {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	get := func(names ...string) string {
		for _, n := range names {
			if v := vals[n]; v != "" {
				return v
			}
		}
		return ""
	}
	return httpproxy.Config{
		HTTPProxy:  get("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: get("HTTPS_PROXY", "https_proxy"),
		NoProxy:    get("NO_PROXY", "no_proxy"),
	}
}

// Audit G22, end to end on the embedded Node: with https_proxy, lowercase and
// with no scheme (a form Go reads as http:// and Node's own agent refuses to
// start on), pointed at a local listener, and an HTTP_PROXY Go does not use
// for https pointed at a dead port, the sidecar's first BotGuard fetch (the homepage, then
// /att/get) arrives at that listener as a CONNECT, the way Go's
// ProxyFromEnvironment clients send theirs. The listener refuses the tunnel,
// so nothing leaves the machine and the mint fails, which is all the test
// needs: the CONNECT is recorded before the refusal is written, so a mint
// that ends with no CONNECT seen went direct.
//
// Mutants: cmd.Env left unset in startLocked (Node's fetch ignores the proxy
// variables, goes straight to www.youtube.com, and the mint ends with no
// CONNECT seen); the raw variables handed to Node with NODE_USE_ENV_PROXY=1
// (Node throws on the scheme-less proxy before server.js runs, and Start
// fails).
func TestTheSidecarFetchesThroughTheEnvironmentsProxy(t *testing.T) {
	requireBlobs(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	connects := make(chan string, 16)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("proxy listener panic: %v", r)
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			select {
			case connects <- strings.TrimSpace(line):
			default:
			}
			_, _ = c.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			c.Close()
		}
	}()

	// Clear first, then set: on Windows a variable's name is not case
	// sensitive, so clearing HTTPS_PROXY after setting https_proxy cleared
	// the proxy itself, and the mint went direct.
	for _, k := range []string{"HTTPS_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY"} {
		t.Setenv(k, "")
	}
	t.Setenv("https_proxy", ln.Addr().String())
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	s := startSidecar(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	minted := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("mint goroutine panic: %v", r)
				minted <- nil
			}
		}()
		_, err := s.GeneratePoToken(ctx, "moombox-env-proxy-test")
		minted <- err
	}()

	select {
	case line := <-connects:
		if !strings.HasPrefix(line, "CONNECT www.youtube.com:443 ") {
			t.Errorf("the proxy's first request = %q, want a CONNECT to www.youtube.com:443", line)
		}
	case err := <-minted:
		t.Fatalf("the mint ended (err=%v) with no request at the proxy: the sidecar's fetches went around HTTPS_PROXY", err)
	}
}

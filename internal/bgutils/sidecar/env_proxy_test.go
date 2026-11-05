package sidecar

import (
	"bufio"
	"context"
	"net"
	"slices"
	"strings"
	"testing"
)

// Audit G22. The Node child's environment is Moombox's own plus
// NODE_USE_ENV_PROXY=1, set last so an inherited NODE_USE_ENV_PROXY=0 cannot
// switch the sidecar off the route Go's clients take.
//
// Mutants: childEnv returning parent unchanged (no flag); the flag prepended
// (an inherited 0 wins, as exec keeps the last value of a key).
func TestChildEnvTurnsOnNodesEnvProxy(t *testing.T) {
	parent := []string{"HTTPS_PROXY=http://proxy.example:3128", "NODE_USE_ENV_PROXY=0", "PATH=/usr/bin"}
	got := childEnv(parent)

	if n := len(got); n == 0 || got[n-1] != "NODE_USE_ENV_PROXY=1" {
		t.Fatalf("childEnv = %q, want NODE_USE_ENV_PROXY=1 as the last entry", got)
	}
	for _, kv := range parent {
		if !slices.Contains(got, kv) {
			t.Errorf("childEnv dropped the parent's %q", kv)
		}
	}
	if parent[1] != "NODE_USE_ENV_PROXY=0" || len(parent) != 3 {
		t.Errorf("childEnv modified its argument: %q", parent)
	}
}

// Audit G22, end to end on the embedded Node: with HTTPS_PROXY pointed at a
// local listener, the sidecar's first BotGuard fetch (the homepage, then
// /att/get) arrives at that listener as a CONNECT, the way Go's
// ProxyFromEnvironment clients send theirs. The listener refuses the tunnel,
// so nothing leaves the machine and the mint fails, which is all the test
// needs: the CONNECT is recorded before the refusal is written, so a mint
// that ends with no CONNECT seen went direct.
//
// Mutant: cmd.Env left unset in startLocked (Node's fetch ignores the proxy
// variables, goes straight to www.youtube.com, and the mint ends with no
// CONNECT seen).
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

	proxyURL := "http://" + ln.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, proxyURL)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY"} {
		t.Setenv(k, "")
	}

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

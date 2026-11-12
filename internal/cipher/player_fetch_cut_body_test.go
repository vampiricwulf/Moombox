package cipher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// cutBodyTransport answers every player GET with a 200. The first `cuts`
// answers carry a body that breaks off after a few bytes with a read error,
// as a reset connection or the client's own timeout does part-way through
// the player's 3 MB; later answers carry `body` whole.
type cutBodyTransport struct {
	cuts int32
	body string
	gets atomic.Int32
}

func (ct *cutBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body io.Reader = strings.NewReader(ct.body)
	if ct.gets.Add(1) <= ct.cuts {
		body = io.MultiReader(strings.NewReader("// half a pla"), cutReader{})
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(body),
		ContentLength: -1,
		Request:       req,
	}, nil
}

type cutReader struct{}

func (cutReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A cold player GET whose 200 body breaks off mid-read is a transport
// failure like a refused connection: it is tried again (owner decision
// PR-R3) and, when every attempt breaks, fails as ErrPlayerJSFetch, which
// the worker reads as systemic. It used to fail untyped after one GET, so
// the worker took it for a fault of the one chosen format and recorded a
// lesser one for the whole capture.
//
// Mutants: the body-read error returned untyped (one GET, and the error is
// not ErrPlayerJSFetch); the half body cached or returned as the player.
func TestAColdPlayerFetchCutMidBodyIsTriedAgain(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	cases := []struct {
		name     string
		cuts     int32
		wantGets int32
		wantErr  bool
	}{
		{"a body that breaks once, then arrives whole", 1, 2, false},
		{"a body that breaks every time", 99, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ct := &cutBodyTransport{cuts: tc.cuts, body: "// the whole player JS"}
				orig := playerHTTPClient
				playerHTTPClient = &http.Client{Transport: ct}
				defer func() { playerHTTPClient = orig }()
				pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
				if err != nil {
					t.Fatal(err)
				}
				js, _, err := pc.Fetch(context.Background(), playerURL)
				if tc.wantErr {
					if !errors.Is(err, ErrPlayerJSFetch) {
						t.Errorf("Fetch = %v, want ErrPlayerJSFetch once every body broke off", err)
					}
					if cached, _ := pc.Get(playerURL); cached != "" {
						t.Errorf("a broken body was cached: %q", cached)
					}
				} else if err != nil || js != ct.body {
					t.Errorf("Fetch = %q, %v; want the whole player from the second GET", js, err)
				}
				if got := ct.gets.Load(); got != tc.wantGets {
					t.Errorf("GETs = %d, want %d", got, tc.wantGets)
				}
			})
		})
	}
}

// With a copy of the player already cached, a revalidation whose 200 body
// breaks off serves that copy, unchanged, as a failed connection does — and
// leaves it on disk for the next caller.
//
// Mutants: the cut body failing the Fetch although a copy is cached; the
// half body replacing the cached copy or reported as a rotation.
func TestAPlayerFetchCutMidBodyServesTheCachedCopy(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	const cachedJS = "// the player cached earlier"
	ct := &cutBodyTransport{cuts: 99}
	orig := playerHTTPClient
	playerHTTPClient = &http.Client{Transport: ct}
	defer func() { playerHTTPClient = orig }()
	pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.Put(playerURL, cachedJS); err != nil {
		t.Fatal(err)
	}
	js, changed, err := pc.Fetch(context.Background(), playerURL)
	if err != nil || js != cachedJS || changed {
		t.Errorf("Fetch = %q, changed %v, %v; want the cached copy, unchanged", js, changed, err)
	}
	if got := ct.gets.Load(); got != 1 {
		t.Errorf("GETs = %d, want 1: a cached copy is served, not retried", got)
	}
	if onDisk, _ := pc.Get(playerURL); onDisk != cachedJS {
		t.Errorf("cache now holds %q, want the copy cached earlier", onDisk)
	}
}

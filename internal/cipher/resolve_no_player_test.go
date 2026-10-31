package cipher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestDirectURLResolvesWithoutAPlayer is the audit's overlay probe
// (yt-pot-jsc-4) made a test: a format whose URL carries neither an encrypted
// signature nor an n asks nothing of a player, and resolves to itself — with
// no player URL at all (the watch page failed and took it down), with no
// solver wired, and with a player URL whose player cannot be fetched. yt-dlp
// sends such a URL as YouTube served it (_video.py:3556-3579); Moombox failed
// it with "player URL is required", or with the player fetch's own error.
//
// The rows below the passthrough pin its edge: an n in the query, an n handed
// in, or an encrypted signature still needs the player, and without one still
// fails as before.
//
// Mutants: the passthrough dropped from RoutedResolveURL (the no-solver row
// fails, "no solver available"; the others still reach ResolveURL's own);
// dropped from GojaResolver.ResolveURL (its direct calls at the end fail);
// needsNoPlayer ignoring the query's n, or NParam, or the signature (the
// matching "still needs" row, or the NParam call, resolves).
func TestDirectURLResolvesWithoutAPlayer(t *testing.T) {
	const direct = "https://rr1---sn-x.googlevideo.com/videoplayback?itag=18&expire=1&c=VISIONOS"

	goja, err := NewGojaResolver(t.TempDir(), nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	defer usePlayerHTTPClient(t)()
	var playerFetches atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		playerFetches.Add(1)
		http.Error(w, "no player here", http.StatusInternalServerError)
	}))
	defer broken.Close()
	unfetchable := broken.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/en_US/base.js"
	routed := &staticSolver{n: map[string]string{"raw-n": "solved-n"}}

	for _, tc := range []struct {
		name      string
		routed    Solver
		goja      *GojaResolver
		playerURL string
		req       ResolveFormatRequest
		wantErr   string // "" — resolves to the URL itself
	}{
		{name: "no player URL, routed and goja", routed: routed, goja: goja, req: ResolveFormatRequest{URL: direct}},
		{name: "no player URL, goja only", goja: goja, req: ResolveFormatRequest{URL: direct}},
		{name: "no player URL, no solver", req: ResolveFormatRequest{URL: direct}},
		{name: "a player that cannot be fetched", goja: goja, playerURL: unfetchable, req: ResolveFormatRequest{URL: direct}},
		{name: "still needs: n in the query", goja: goja, req: ResolveFormatRequest{URL: direct + "&n=raw-n"}, wantErr: "player URL is required"},
		{name: "still needs: an encrypted signature", goja: goja, req: ResolveFormatRequest{URL: direct, EncryptedSig: "ENC"}, wantErr: "player URL is required"},
		{name: "still needs: no solver for an n", req: ResolveFormatRequest{URL: direct + "&n=raw-n"}, wantErr: "no solver available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveFormatURL(context.Background(), tc.routed, tc.goja, tc.playerURL, tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ResolveFormatURL = %q, %v; want an error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveFormatURL: %v — a URL that asks nothing of a player must resolve without one", err)
			}
			if got != direct {
				t.Errorf("ResolveFormatURL = %q, want the URL unchanged (%q)", got, direct)
			}
		})
	}
	if n := playerFetches.Load(); n != 0 {
		t.Errorf("the player was fetched %d times for URLs that need none of it", n)
	}

	// GojaResolver.ResolveURL keeps the same contract on its own: today only
	// RoutedResolveURL calls it, after its own passthrough, but it is an
	// exported method and its answer must not depend on who asked.
	for _, playerURL := range []string{"", unfetchable} {
		res, err := goja.ResolveURL(context.Background(), ResolveURLRequest{StreamURL: direct, PlayerURL: playerURL})
		if err != nil || res.URL != direct {
			t.Errorf("GojaResolver.ResolveURL(player %q) = %+v, %v; want the URL unchanged", playerURL, res, err)
		}
	}
	if n := playerFetches.Load(); n != 0 {
		t.Errorf("GojaResolver.ResolveURL fetched the player %d times for a URL that needs none of it", n)
	}
	// NParam is the other way an n reaches ResolveURL.
	if _, err := goja.ResolveURL(context.Background(), ResolveURLRequest{StreamURL: direct, NParam: "raw-n"}); err == nil ||
		!strings.Contains(err.Error(), "player URL is required") {
		t.Errorf("ResolveURL with NParam and no player = %v, want \"player URL is required\"", err)
	}
}

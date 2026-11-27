package youtube

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The live tests of today's player and of a format URL YouTube serves (owner
// ruling RV-15). Neither prints a URL: a format URL carries the signature and
// the requester's address. Only itags, sources, sizes, statuses and whether
// a value changed are logged.

// liveSolvers starts a real sidecar and the goja resolver and returns them
// with the production routing over both (cipher.NewCompositeSolver over
// NewSidecarSolver, as cmd/moombox wires it).
func liveSolvers(t *testing.T, ctx context.Context) (*sidecar.Sidecar, *cipher.GojaResolver, cipher.Solver, cipher.Solver) {
	t.Helper()
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}
	goja, err := cipher.NewGojaResolver(t.TempDir(), noopLogger{})
	if err != nil {
		t.Fatalf("goja resolver: %v", err)
	}
	sc := sidecar.New(sidecar.Config{
		CacheDir:       t.TempDir(),
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
		Logger:         noopLogger{},
	})
	if err := sc.Start(ctx); err != nil {
		t.Fatalf("sidecar Start: %v", err)
	}
	t.Cleanup(func() { _ = sc.Stop() })
	side := cipher.NewSidecarSolver(sc, goja)
	return sc, goja, side, cipher.NewCompositeSolver(side, goja)
}

// TestLiveCurrentPlayerSolves takes today's player (the id /iframe_api names,
// yt-dlp's _download_player_url), fetches its main and tv bodies, and asks
// the sidecar to solve an n and a signature on each body, then through the
// production path (main body first, the tv body when ejs cannot solve with
// it; owner ruling RV-11). It fails when the production path gives no real
// answer: the day YouTube ships a player neither body of which ejs can
// prepare. Goja's answer on the same player is compared where goja can solve
// it; where it cannot, that is logged, not failed (goja is the fallback).
//
// Enable with MOOMBOX_LIVE_YT_TEST=1 (network; the sidecar embed blobs).
func TestLiveCurrentPlayerSolves(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_YT_TEST=1 to solve today's player live")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	sc, goja, side, _ := liveSolvers(t, ctx)

	playerURL, err := fetchPlayerURLFromIframeAPI(ctx, "")
	if err != nil {
		t.Fatalf("iframe API: %v", err)
	}
	id := cipher.PlayerIDOf(playerURL)
	if id == "" {
		t.Fatal("the iframe API named a URL with no player id")
	}
	t.Logf("player %s", id)

	const sigIn = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_.0123456789ab"
	const nIn = "abcdefghij_12345"

	mainJS, err := goja.PlayerJS(id)
	if err != nil {
		t.Fatalf("main body fetch: %v", err)
	}
	tvJS, err := goja.TVPlayerJS(ctx, id)
	if err != nil {
		t.Fatalf("tv body fetch: %v", err)
	}
	t.Logf("bodies: main %d bytes, tv %d bytes", len(mainJS), len(tvJS))

	solved := 0
	for _, b := range []struct{ name, key, js string }{{"main", id + "-live-main", mainJS}, {"tv", id + "-live-tv", tvJS}} {
		res, err := sc.SolveCipher(ctx, sidecar.SolveCipherRequest{
			PlayerID: b.key, PlayerJS: b.js, SigChallenges: []string{sigIn}, NChallenges: []string{nIn},
		})
		if err != nil {
			t.Logf("%s body: ejs could not solve: %.200v", b.name, err)
			continue
		}
		sig, n := res.SigResults[sigIn], res.NResults[nIn]
		ok := sig != "" && sig != sigIn && n != "" && n != nIn
		t.Logf("%s body: sig changed %t, n changed %t", b.name, sig != "" && sig != sigIn, n != "" && n != nIn)
		if ok {
			solved++
		}
	}
	if solved == 0 {
		t.Error("ejs solved n and sig on neither the main nor the tv body of today's player")
	}

	gotSig, err := side.Sig(ctx, id, sigIn)
	if err != nil || gotSig == "" || gotSig == sigIn {
		t.Errorf("production path sig: err=%.200v, changed=%t", err, gotSig != "" && gotSig != sigIn)
	}
	gotN, err := side.N(ctx, id, nIn)
	if err != nil || gotN == "" || gotN == nIn {
		t.Errorf("production path n: err=%.200v, changed=%t", err, gotN != "" && gotN != nIn)
	}

	g, err := goja.DecryptSignature(ctx, cipher.SignatureRequest{EncryptedSignature: sigIn, NParam: nIn, PlayerURL: playerURL})
	switch {
	case err != nil:
		// Since RV-12 goja runs a port of the same ejs solver, so a player the
		// sidecar answers and goja does not is a port that has fallen behind.
		t.Errorf("goja cannot solve today's player although the sidecar can (extractor mismatch=%t): %.200v",
			errors.Is(err, cipher.ErrExtractorMismatch), err)
	default:
		if (g.DecryptedSignature == "") != (gotSig == "") || (g.DecryptedNSig == "") != (gotN == "") {
			t.Errorf("goja and the sidecar did not answer the same challenges: goja sig %t n %t, sidecar sig %t n %t",
				g.DecryptedSignature != "", g.DecryptedNSig != "", gotSig != "", gotN != "")
		}
		if g.DecryptedSignature != "" && gotSig != "" && g.DecryptedSignature != gotSig {
			t.Error("goja and the sidecar disagree on the signature of today's player")
		}
		if g.DecryptedNSig != "" && gotN != "" && g.DecryptedNSig != gotN {
			t.Error("goja and the sidecar disagree on the n of today's player")
		}
		t.Logf("goja: sig answered %t, n answered %t; parity checked where both answered",
			g.DecryptedSignature != "", g.DecryptedNSig != "")
	}
}

// TestLiveFormatURLServes extracts a stable video anonymously and, with a
// session, signed in, resolves an audio format URL the way the worker does
// (cipher.ResolveFormatURL over the production solvers) and asks YouTube for
// its first KiB. A 403 here is what the format counts other live tests check
// cannot see: a URL that lists fine and is refused when fetched (an n or a
// signature solved wrong, a client's URLs newly needing a token). Formats
// whose client needs a GVS PO token (GvsTokenRequired) are left out: minting
// one is the bgutils group's test. A format that needs the player's JS is
// preferred, so the solve is on the path; one that does not is checked too
// when the extraction has one.
//
// Enable with MOOMBOX_LIVE_YT_TEST=1; the signed-in half also needs
// MOOMBOX_LIVE_YT_COOKIES.
func TestLiveFormatURLServes(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_YT_TEST=1 to fetch a format URL live")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	_, goja, _, routed := liveSolvers(t, ctx)

	check := func(t *testing.T, info *VideoInfo) {
		var withJS, without *Format
		for i := range info.Formats {
			f := &info.Formats[i]
			if !f.IsAudio() || f.IsVideo() || GvsTokenRequired(f.Source) || (f.URL == "" && f.EncryptedSig == "") {
				continue
			}
			if f.NeedsPlayerJS() {
				if withJS == nil {
					withJS = f
				}
			} else if without == nil {
				without = f
			}
		}
		if withJS == nil && without == nil {
			t.Skipf("the extraction (%d formats) kept no audio format whose client needs no GVS token", len(info.Formats))
		}
		for _, f := range []*Format{withJS, without} {
			if f == nil {
				continue
			}
			u, err := cipher.ResolveFormatURL(ctx, routed, goja, info.PlayerURL, cipher.ResolveFormatRequest{
				URL: f.URL, EncryptedSig: f.EncryptedSig, SigKey: f.SigKey,
			})
			if err != nil {
				t.Errorf("itag %d (%s): resolve failed: %.200v", f.Itag, f.Source, err)
				continue
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			req.Header.Set("User-Agent", constants.UserAgents.Web)
			req.Header.Set("Range", "bytes=0-1023")
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			if err != nil {
				t.Errorf("itag %d (%s): GET failed (URL not shown)", f.Itag, f.Source)
				continue
			}
			resp.Body.Close()
			t.Logf("itag %d (%s, needs player JS %t): status %d", f.Itag, f.Source, f.NeedsPlayerJS(), resp.StatusCode)
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				t.Errorf("itag %d (%s): YouTube answered %d to the first KiB", f.Itag, f.Source, resp.StatusCode)
			}
		}
	}

	t.Run("anonymous", func(t *testing.T) {
		svc := NewService(cookies.NewCookieJar(), noopLogger{})
		svc.PlayerAPI.SetCipherSolver(goja)
		info, err := svc.PlayerAPI.GetVideoInfoPublic(ctx, "aqz-KE-bpKQ")
		if err != nil {
			t.Fatalf("GetVideoInfoPublic: %.300v", err)
		}
		check(t, info)
	})
	t.Run("signed_in", func(t *testing.T) {
		path := liveCookieFile(t, "MOOMBOX_LIVE_YT_COOKIES")
		if path == "" {
			t.Skip("set MOOMBOX_LIVE_YT_COOKIES=<path to a signed-in Netscape cookie file> for the signed-in half")
		}
		jar := cookies.NewCookieJar()
		if err := jar.Load(path); err != nil {
			t.Fatalf("load cookie file: %v", err)
		}
		svc := NewService(jar, noopLogger{})
		if !svc.HasAnyAuthCookie() {
			t.Fatal("that cookie file carries no YouTube auth cookie")
		}
		svc.PlayerAPI.SetCipherSolver(goja)
		info, err := svc.PlayerAPI.GetVideoInfoAuthenticated(ctx, "aqz-KE-bpKQ")
		if err != nil {
			t.Fatalf("GetVideoInfoAuthenticated: %.300v", err)
		}
		check(t, info)
	})
}

// TestLiveMembershipTabParses fetches a channel's /membership tab with the
// session, through both readers the feed monitor and the backfill scan use
// (FetchMembershipVideos, FetchChannelTabPage). The account need not be a
// member: what is checked is that the page comes back signed in, that both
// readers take it without an error, and what they read (counts only). A
// login wall, or a reader that no longer recognises the page, fails here
// rather than as members-only streams that are silently never found.
//
// Enable with MOOMBOX_LIVE_YT_COOKIES=<path to a signed-in Netscape cookie
// file>.
func TestLiveMembershipTabParses(t *testing.T) {
	path := liveCookieFile(t, "MOOMBOX_LIVE_YT_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_YT_COOKIES=<path to a signed-in Netscape cookie file> to read a membership tab live")
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	svc := NewService(jar, noopLogger{})
	if !svc.HasAnyAuthCookie() {
		t.Fatal("that cookie file carries no YouTube auth cookie")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A large channel with memberships on: Hololive's Usada Pekora.
	const channel = "UC1DCedRgGHBdm81E1llLhOQ"
	vids, auth, access, err := svc.FetchMembershipVideos(ctx, channel)
	if err != nil {
		t.Fatalf("membership page: %.300v", err)
	}
	t.Logf("membership page: session %s, member access %t, %d videos", auth, access, len(vids))
	if auth != SessionAuthLoggedIn {
		t.Errorf("the membership page's session verdict = %q, want %q", auth, SessionAuthLoggedIn)
	}

	page, err := svc.FetchChannelTabPage(ctx, channel, "membership", "")
	if err != nil {
		t.Fatalf("membership tab through /browse: %.300v", err)
	}
	t.Logf("membership tab through /browse: %d items, more pages %t", len(page.Items), page.Continuation != "")
	if access && len(vids) > 0 && len(page.Items) == 0 {
		t.Error("the page lists members-only videos but the /browse reader read none")
	}
}

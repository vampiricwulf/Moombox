package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The whole-file refresh's token fallback (refreshVodURL), driven through the
// closure DownloadVod builds (vodURLRefresh) on a fake clock (vodRefreshNow):
// every refusal below lands at an instant the test chose, so "long before
// the URL's expiry" and "near it" are exact, and no test waits on a wall
// clock. Each re-fetch hands out the next generation of URLs, so which call
// produced a URL reads off the URL itself.

const fallbackPlayer = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"

const fallbackLineMsg = "[POT] VOD URL refresh: a token-free URL was refused long before its expiry — falling back to a GVS token"

// fallbackURL is generation gen's URL for client dir ("tv" or "web") of the
// itag-137 file, expiring at exp — or stating no expiry when exp is zero.
func fallbackURL(dir string, gen int, exp time.Time) string {
	q := ""
	if !exp.IsZero() {
		q = "expire=" + strconv.FormatInt(exp.Unix(), 10) + "&"
	}
	return fmt.Sprintf("http://cdn.invalid/%s/videoplayback?%sid=o-%s%d&itag=137&clen=1000", dir, q, dir, gen)
}

// fallbackPool is a fresh extraction's pool for the itag-137 file, run
// through the real dedup: the tv_auth copy wins, carrying the watch page's
// copy as its token-carrying shadow unless withCopy is false.
func fallbackPool(gen int, exp time.Time, withCopy bool) []youtube.Format {
	copies := []youtube.Format{withLevel(youtube.Format{Itag: 137, URL: fallbackURL("tv", gen, exp), ContentLength: "1000", Source: "tv_auth"}, youtube.AuthLevelTVAuth)}
	if withCopy {
		copies = append(copies, withLevel(youtube.Format{Itag: 137, URL: fallbackURL("web", gen, exp), ContentLength: "1000", Source: "watch_page"}, youtube.AuthLevelWatchPageAuth))
	}
	return youtube.MergeFormatPool(context.Background(), nil, copies)
}

// fallbackRig is one tv_auth stream's refresh on the fake clock.
type fallbackRig struct {
	t    *testing.T
	job  *JobContext
	logs *captureLogger
	// now is the fake clock; a test moves it between refreshes.
	now time.Time
	// freshLife is how long each fresh URL lives from its re-fetch; zero
	// hands out URLs that state no expiry.
	freshLife time.Duration
	withCopy  bool
	// pool, when set, is the fresh pool in fallbackPool's place, and source
	// the served stream's client in tv_auth's.
	pool   func(gen int, exp time.Time) []youtube.Format
	source string
	// failFetch makes every re-fetch fail.
	failFetch bool
	gen       int
	// The fake minter: each mint's bypassCache and content binding, and what
	// it answers.
	mints    []bool
	bindings []string
	mintErr  error
	empty    bool
	minter   gvsTokenMinter
	// info is the extraction start built the stream from.
	info    *youtube.VideoInfo
	refresh func() (string, string)
}

func newFallbackRig(t *testing.T) *fallbackRig {
	t.Helper()
	r := &fallbackRig{t: t, now: time.Unix(1_800_000_000, 0), freshLife: 6 * time.Hour, withCopy: true, source: "tv_auth"}
	orig := vodRefreshNow
	vodRefreshNow = func() time.Time { return r.now }
	t.Cleanup(func() { vodRefreshNow = orig })
	stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
		r.gen++
		var exp time.Time
		if r.freshLife > 0 {
			exp = r.now.Add(r.freshLife)
		}
		if r.failFetch {
			return nil // the re-fetch failed
		}
		formats := fallbackPool(r.gen, exp, r.withCopy)
		if r.pool != nil {
			formats = r.pool(r.gen, exp)
		}
		return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: fallbackPlayer, Formats: formats}
	})
	r.job, r.logs = vodPotJob(t)
	r.job.YT = youtube.NewService(nil, &discardLogger{})
	r.minter = &fakePotProvider{generate: func(_ context.Context, binding string, bypassCache bool) (string, error) {
		r.mints = append(r.mints, bypassCache)
		r.bindings = append(r.bindings, binding)
		if r.mintErr != nil {
			return "", r.mintErr
		}
		if r.empty {
			return "", nil
		}
		return "tok" + strconv.Itoa(len(r.mints)), nil
	}}
	return r
}

// start builds the stream's refresh the way DownloadVod does: served is the
// r.source copy whose resolved URL is servedURL, from an extraction whose
// verdict exempt is, its own pre-roll ad over at adEnd. The extraction
// carries a signed-in session's datasync binding, which every mint must use.
func (r *fallbackRig) start(servedURL string, adEnd time.Time, exempt bool) {
	served := youtube.Format{Itag: 137, URL: servedURL, ContentLength: "1000", Source: r.source, AvailableAt: adEnd}
	r.info = &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlaybackWaitExempt: exempt, PlayerURL: fallbackPlayer, Formats: []youtube.Format{served},
		GvsBinding: "datasync-0123", GvsBindingKind: youtube.BindingDataSyncID}
	r.refresh = vodURLRefresh(context.Background(), r.job, r.info, served, servedURL, stubCipherSolver{}, nil, r.minter, "VOD video")
}

// checkBindings fails unless every mint so far was bound as every other GVS
// mint of the job is (gvsBinding of the extraction the stream came from).
func (r *fallbackRig) checkBindings() {
	r.t.Helper()
	want, _ := gvsBinding(r.job, r.info)
	for i, b := range r.bindings {
		if b != want {
			r.t.Errorf("mint %d bound to %q, want %q (gvsBinding)", i+1, b, want)
		}
	}
}

// expect runs one refresh and checks what it hands the engine and what its
// closing "[POT] VOD URL refresh" line says: the step, the client, newToken.
func (r *fallbackRig) expect(wantURL, wantToken string, wantStep int, wantSource string) {
	r.t.Helper()
	before := len(potLines(r.logs, "[POT] VOD URL refresh"))
	gotURL, gotToken := r.refresh()
	if gotURL != wantURL || gotToken != wantToken {
		r.t.Fatalf("refresh = (%q, %q), want (%q, %q)", gotURL, gotToken, wantURL, wantToken)
	}
	lines := potLines(r.logs, "[POT] VOD URL refresh")
	if len(lines) != before+1 {
		r.t.Fatalf("refresh logged %d closing lines, want 1", len(lines)-before)
	}
	l := lines[len(lines)-1]
	if l["step"] != wantStep || l["source"] != wantSource || l["newToken"] != (wantToken != "") || l["tag"] != "VOD video" {
		r.t.Errorf("closing line = %v, want step=%d source=%s newToken=%v tag=VOD video", l, wantStep, wantSource, wantToken != "")
	}
}

// fallbackLines are the lines saying the fallback ran.
func (r *fallbackRig) fallbackLines() []map[string]any { return potLines(r.logs, fallbackLineMsg) }

// TestVodTokenFallbackLeavesAnExpiryAlone: a token-free stream's URL refused
// at, past or near the expiry it states — or, stating none, an hour or more
// after it was handed out — gets the refresh it always got: a fresh URL of
// the same class, bare, and no mint. This is the refusal the refresh was
// built for, and the fallback must not touch it.
//
// Mutant: dropping the expiry check from fallbackStep (every refusal reads as
// early) — each row mints and hands out a token. Mutant:
// vodRefusalExpiryMargin at 0 — the "ten minutes left" row escalates.
// Mutant: dropping the age rule (a URL with no expiry always early) — the
// age row escalates.
func TestVodTokenFallbackLeavesAnExpiryAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		served  func(now time.Time) string
		refused time.Duration // after the hand-out
	}{
		{"past its expiry", func(now time.Time) string { return fallbackURL("tv", 0, now.Add(-time.Hour)) }, 0},
		{"ten minutes from its expiry", func(now time.Time) string { return fallbackURL("tv", 0, now.Add(10*time.Minute)) }, 0},
		{"no expiry stated, refused two hours on", func(time.Time) string { return fallbackURL("tv", 0, time.Time{}) }, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFallbackRig(t)
			r.start(tc.served(r.now), time.Time{}, false)
			r.now = r.now.Add(tc.refused)
			r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "", 0, "tv_auth")
			if len(r.mints) != 0 {
				t.Errorf("minted %d tokens for an expiry", len(r.mints))
			}
			if l := r.fallbackLines(); len(l) != 0 {
				t.Errorf("logged the fallback for an expiry: %v", l)
			}
		})
	}
}

// TestVodTokenFallbackSteps is the owner's design, step by step, for the
// field case: a tv_auth URL refused hours before its expiry. The first such
// refusal hands out a fresh tv_auth URL with a token minted past the cache
// (step 1); the second, the same file from the watch page — the fresh pool's
// token-carrying copy — with a fresh token (step 2). From then on the stream
// rides that copy, and a refusal is the ordinary token-requiring refresh:
// minted, same class, no further step. Each step logs which ran, the client
// and newToken, and one line saying why it fell back.
//
// Mutant: dropping the fallback from refreshVodURL (step always 0) — call 1
// is bare. Mutant: step 2 skipped (min(st.step+1, 1)) — call 2 stays on
// tv_auth. Mutant: st.served not moved onto the copy (handOut's class
// switch dropped) — call 3 asks for a tv copy with the token class of
// tv_auth, and the closing line says step 2 again. Mutant: bypassCache false
// on the mint — the mint check. Mutant: the mint bound to "" instead of the
// job's binding — the binding check.
func TestVodTokenFallbackSteps(t *testing.T) {
	r := newFallbackRig(t)
	r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)

	r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
	r.now = r.now.Add(time.Minute)
	r.expect(fallbackURL("web", 2, r.now.Add(6*time.Hour)), "tok2", 2, "watch_page")
	r.now = r.now.Add(time.Minute)
	r.expect(fallbackURL("web", 3, r.now.Add(6*time.Hour)), "tok3", 0, "watch_page")

	if want := []bool{true, true, true}; fmt.Sprint(r.mints) != fmt.Sprint(want) {
		t.Errorf("mints' bypassCache = %v, want %v — the cached token may be the refused one", r.mints, want)
	}
	r.checkBindings()
	lines := r.fallbackLines()
	if len(lines) != 2 {
		t.Fatalf("fallback lines = %v, want one per step", lines)
	}
	for i, l := range lines {
		if l["step"] != i+1 || l["source"] != "tv_auth" || l["expiresIn"] == nil {
			t.Errorf("fallback line %d = %v, want step=%d source=tv_auth with the time it still had", i+1, l, i+1)
		}
	}
}

// TestVodTokenFallbackKeepsAWorkingToken: once step 1's token rides the
// stream, the refresh that meets the URL's real expiry hours later mints one
// again for the fresh URL rather than sending it bare — the URLs worked with
// a token, and the engine could not clear one anyway. A step whose re-fetch
// fails hands out no URL, and its token rides the current one (keep): that
// is step 1 run and a token on the stream as much as a fresh URL with one
// is, so the expiry still mints and the next early refusal is step 2.
//
// Mutant: noteToken not recording st.tokenised — each expiry's refresh comes
// back bare. Mutant: keep() not recording the token (st.noteToken dropped
// from it) — the "left on the current URL" row's expiry comes back bare, and
// its next early refusal runs step 1 again instead of moving onto the watch
// page's copy.
func TestVodTokenFallbackKeepsAWorkingToken(t *testing.T) {
	t.Run("on a fresh URL", func(t *testing.T) {
		r := newFallbackRig(t)
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
		r.now = r.now.Add(6*time.Hour - 15*time.Minute) // fifteen minutes before tv1's expiry
		r.expect(fallbackURL("tv", 2, r.now.Add(6*time.Hour)), "tok2", 0, "tv_auth")
	})
	t.Run("left on the current URL", func(t *testing.T) {
		r := newFallbackRig(t)
		r.failFetch = true
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		if u, tok := r.refresh(); u != "" || tok != "tok1" {
			t.Fatalf("step 1 with the re-fetch failing = (%q, %q), want (\"\", \"tok1\") — the token on the current URL", u, tok)
		}
		r.failFetch, r.withCopy = false, false
		r.now = r.now.Add(5*time.Hour - 15*time.Minute) // fifteen minutes before tv0's expiry
		r.expect(fallbackURL("tv", 2, r.now.Add(6*time.Hour)), "tok2", 0, "tv_auth")
		r.withCopy = true
		r.expect(fallbackURL("web", 3, r.now.Add(6*time.Hour)), "tok3", 2, "watch_page")
	})
}

// TestVodTokenFallbackReadsTheURLItHandedOut: the rule reads the URL the
// engine is actually refusing — the one the last refresh handed out, and
// when — not the one the download started on.
//
// Mutant: handOut not recording st.url — the "short-lived fresh URL" row
// reads the setup URL's five hours and runs step 2. Mutant: handOut not
// recording st.issued — the "no expiry" row reads an age of 80 minutes from
// the setup hand-out, calls it the expiry, and stays on step 1's shape.
func TestVodTokenFallbackReadsTheURLItHandedOut(t *testing.T) {
	t.Run("a short-lived fresh URL", func(t *testing.T) {
		r := newFallbackRig(t)
		r.freshLife = 20 * time.Minute
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		r.expect(fallbackURL("tv", 1, r.now.Add(20*time.Minute)), "tok1", 1, "tv_auth")
		// tv1 has twenty minutes left: its refusal is the expiry.
		r.expect(fallbackURL("tv", 2, r.now.Add(20*time.Minute)), "tok2", 0, "tv_auth")
	})
	t.Run("no expiry stated", func(t *testing.T) {
		r := newFallbackRig(t)
		r.freshLife = 0
		r.start(fallbackURL("tv", 0, time.Time{}), time.Time{}, false)
		r.now = r.now.Add(50 * time.Minute)
		r.expect(fallbackURL("tv", 1, time.Time{}), "tok1", 1, "tv_auth")
		// tv1 was handed out half an hour ago: early, so step 2.
		r.now = r.now.Add(30 * time.Minute)
		r.expect(fallbackURL("web", 2, time.Time{}), "tok2", 2, "watch_page")
		if l := r.fallbackLines(); len(l) != 2 || l[1]["age"] != "30m0s" {
			t.Errorf("fallback lines = %v, want step 2's to say the URL was 30m0s old", l)
		}
	})
}

// TestVodTokenFallbackWithNoTokenCarryingCopy: a fresh pool with no
// token-requiring copy of the file — no web-family client returned it with a
// URL — gives step 2 nothing to move to. It hands out step 1's shape again,
// says why, and looks again on the next early refusal, moving as soon as a
// copy turns up.
//
// Mutant: dropping the no-copy Info line. Mutant: the step left uncapped
// (st.step+1) — call 3 is a "step 3" that looks for no copy and stays on
// tv_auth.
func TestVodTokenFallbackWithNoTokenCarryingCopy(t *testing.T) {
	r := newFallbackRig(t)
	r.withCopy = false
	r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
	r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
	r.expect(fallbackURL("tv", 2, r.now.Add(6*time.Hour)), "tok2", 2, "tv_auth")
	l := logLine(r.logs, "[POT] VOD URL refresh: no token-carrying copy of the same file")
	if l == nil || l["itag"] != 137 || l["clen"] != "1000" || l["source"] != "tv_auth" {
		t.Errorf("no-copy line = %v, want one naming itag 137, clen 1000 and the tv_auth stream", l)
	}
	r.withCopy = true
	r.expect(fallbackURL("web", 3, r.now.Add(6*time.Hour)), "tok3", 2, "watch_page")
}

// TestVodTokenFallbackWithoutAToken: when no token can be minted — no
// provider wired (a nil interface, or the typed-nil *bgutils.PotProvider
// DownloadVod hands over when the provider is disabled), a mint that fails,
// or one that comes back empty — no step can run. The refresh is the one it
// always was, bare, and a Warn says the fallback could not run and why; the
// next early refusal tries again.
//
// Mutant: the Warn dropped. Mutant: `step = 0` dropped after a failed mint —
// the closing line says step 1 for a bare URL. Mutant: minterUsable dropped
// from the mint (a typed-nil provider is called) — the typed-nil row panics.
func TestVodTokenFallbackWithoutAToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rig     func(r *fallbackRig)
		wantErr string
	}{
		{"no provider", func(r *fallbackRig) { r.minter = nil }, errNoGvsMinter.Error()},
		{"a disabled provider", func(r *fallbackRig) { r.minter = vodRefreshMinter(nil) }, errNoGvsMinter.Error()},
		{"a mint that fails", func(r *fallbackRig) { r.mintErr = errors.New("sidecar down") }, "sidecar down"},
		{"an empty token", func(r *fallbackRig) { r.empty = true }, errEmptyGvsToken.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFallbackRig(t)
			tc.rig(r)
			r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
			r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "", 0, "tv_auth")
			r.expect(fallbackURL("tv", 2, r.now.Add(6*time.Hour)), "", 0, "tv_auth")
			warns := potLines(r.logs, "[POT] VOD URL refresh: no GVS token to fall back to — refreshing the token-free URL bare")
			if len(warns) != 2 {
				t.Fatalf("no-token Warns = %v, want one per refusal", warns)
			}
			if err, _ := warns[0]["err"].(error); err == nil || err.Error() != tc.wantErr || warns[0]["step"] != 1 {
				t.Errorf("no-token Warn = %v, want step=1 err=%q", warns[0], tc.wantErr)
			}
		})
	}
}

// TestVodTokenFallbackSparesTheAdWindow: a URL refused before its own
// pre-roll ad was over was refused for the ad, not for a token, so it gets a
// fresh URL of its class — which waits out its own ad — and no fallback
// step. That holds only while the URL's ad is one a download waits for: an
// extraction upstream calls live or post-live waits for none, so its URL has
// no ad window to be refused inside, and an early refusal there runs step 1.
//
// The window is the one of the URL being refused: a refreshed URL's is its
// own response's, not the setup URL's.
//
// Mutant: dropping the ad-window check from fallbackStep — the first row
// runs step 1. Mutant: adWindowEnd ignoring the verdict — the exempt row
// reads the refusal as the ad and stays bare. Mutant: handOut not recording
// st.adEnd — the refreshed-URL row reads the setup URL's window, which is
// over, and runs step 2.
func TestVodTokenFallbackSparesTheAdWindow(t *testing.T) {
	t.Run("inside the ad window", func(t *testing.T) {
		r := newFallbackRig(t)
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), r.now.Add(10*time.Second), false)
		r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "", 0, "tv_auth")
		if l := logLine(r.logs, "[POT] VOD URL refresh: refused inside its own pre-roll ad"); l == nil || l["adLeft"] != "10s" {
			t.Errorf("ad-window line = %v, want one with adLeft=10s", l)
		}
		if len(r.mints) != 0 || len(r.fallbackLines()) != 0 {
			t.Errorf("an ad-window refusal ran the fallback: %d mints, lines %v", len(r.mints), r.fallbackLines())
		}
	})
	t.Run("an extraction nothing waits for", func(t *testing.T) {
		r := newFallbackRig(t)
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), r.now.Add(10*time.Second), true)
		r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
	})
	t.Run("inside a refreshed URL's own ad window", func(t *testing.T) {
		r := newFallbackRig(t)
		// The fake clock sits in the real past, so each fresh copy's ad, ten
		// seconds on from it, is long over by the wall clock the refresh's
		// own wait reads (waitForFreshPlayback) and the refresh returns at
		// once, while the rule reads the fake clock and sees the ad ahead.
		r.now = time.Unix(1_700_000_000, 0)
		r.pool = func(gen int, exp time.Time) []youtube.Format {
			ad := r.now.Add(10 * time.Second)
			return youtube.MergeFormatPool(context.Background(), nil, []youtube.Format{
				withLevel(youtube.Format{Itag: 137, URL: fallbackURL("tv", gen, exp), ContentLength: "1000", Source: "tv_auth", AvailableAt: ad}, youtube.AuthLevelTVAuth),
				withLevel(youtube.Format{Itag: 137, URL: fallbackURL("web", gen, exp), ContentLength: "1000", Source: "watch_page", AvailableAt: ad}, youtube.AuthLevelWatchPageAuth),
			})
		}
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
		// tv1 refused at the same instant: inside its own ad, though the
		// setup URL had none. Tokenised by step 1, so the fresh URL mints.
		r.expect(fallbackURL("tv", 2, r.now.Add(6*time.Hour)), "tok2", 0, "tv_auth")
		if l := logLine(r.logs, "[POT] VOD URL refresh: refused inside its own pre-roll ad"); l == nil || l["adLeft"] != "10s" {
			t.Errorf("ad-window line = %v, want one with adLeft=10s", l)
		}
	})
}

// TestVodTokenFallbackSparesATokenRequiringStream: a stream whose client
// needs the token from the start already mints on every refresh, and an
// early refusal of it is no reason to change anything: same class, fresh
// token, no fallback line. Its fresh copy behind a token-free winner is the
// winner's TokenAlternate, which sameVodFile reads.
//
// Mutant: sameVodFile not reading TokenAlternate — the refresh finds no
// watch_page copy and keeps the current URL.
func TestVodTokenFallbackSparesATokenRequiringStream(t *testing.T) {
	r := newFallbackRig(t)
	servedURL := fallbackURL("web", 0, r.now.Add(5*time.Hour))
	served := withLevel(youtube.Format{Itag: 137, URL: servedURL, ContentLength: "1000", Source: "watch_page"}, youtube.AuthLevelWatchPageAuth)
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: fallbackPlayer, Formats: []youtube.Format{served}}
	r.refresh = vodURLRefresh(context.Background(), r.job, info, served, servedURL, stubCipherSolver{}, nil, r.minter, "VOD video")
	r.expect(fallbackURL("web", 1, r.now.Add(6*time.Hour)), "tok1", 0, "watch_page")
	if l := r.fallbackLines(); len(l) != 0 {
		t.Errorf("a token-requiring stream ran the fallback: %v", l)
	}
}

// TestVodTokenFallbackPutsNoWebPoOnANonWebPoClient: a URL from a client
// upstream binds no WebPO to — visionos and android_vr, outside pot/utils.py's
// WEBPO_CLIENTS — never carries one, and the engine could never take one off
// again (an android_vr URL with one is the 2026-08-15 403 cause). So a
// missing_pot stream of either client refused early skips step 1: it moves
// straight onto the token-carrying copy of its file or, with none, is
// refreshed bare. A refresh whose re-fetch fails leaves no token on its
// current URL, where a tv_auth stream's leaves step 1's token on the URL it
// already has.
//
// Mutant: dropping the takesNoWebPo jump in fallbackStep — the copy rows'
// fallback line says step 1 (the hand-out check still moves them onto the
// copy, TestVodTokenFallbackJudgesTheURLItHandsOut). Mutant: takesNoWebPo
// back to android_vr alone — every visionos row puts a token on a visionos
// URL. Mutant: keep() handing out the token whatever the client — the failed
// re-fetch rows' visionos and android_vr streams get one.
func TestVodTokenFallbackPutsNoWebPoOnANonWebPoClient(t *testing.T) {
	for _, c := range []struct{ source, dir string }{{"android_vr", "avr"}, {"visionos", "vos"}} {
		own := func(gen int, exp time.Time) youtube.Format {
			level := youtube.AuthLevelAndroidVR
			if c.source == "visionos" {
				level = youtube.AuthLevelVisionOS
			}
			return withLevel(youtube.Format{Itag: 137, URL: fallbackURL(c.dir, gen, exp), ContentLength: "1000", Source: c.source}, level)
		}
		t.Run(c.source+", onto the token-carrying copy at once", func(t *testing.T) {
			r := newFallbackRig(t)
			r.source = c.source
			r.pool = func(gen int, exp time.Time) []youtube.Format {
				return youtube.MergeFormatPool(context.Background(), nil, []youtube.Format{webCreatorCopy(gen, exp), own(gen, exp)})
			}
			r.start(fallbackURL(c.dir, 0, r.now.Add(5*time.Hour)), time.Time{}, false)
			r.expect(fallbackURL("wc", 1, r.now.Add(6*time.Hour)), "tok1", 2, "web_creator")
			if l := r.fallbackLines(); len(l) != 1 || l[0]["step"] != 2 || l[0]["source"] != c.source {
				t.Errorf("fallback lines = %v, want one, step 2, for the %s stream", l, c.source)
			}
		})
		t.Run(c.source+", no copy: refreshed bare", func(t *testing.T) {
			r := newFallbackRig(t)
			r.source = c.source
			r.pool = func(gen int, exp time.Time) []youtube.Format { return []youtube.Format{own(gen, exp)} }
			r.start(fallbackURL(c.dir, 0, r.now.Add(5*time.Hour)), time.Time{}, false)
			r.expect(fallbackURL(c.dir, 1, r.now.Add(6*time.Hour)), "", 0, c.source)
			r.expect(fallbackURL(c.dir, 2, r.now.Add(6*time.Hour)), "", 0, c.source)
			if l := logLine(r.logs, noCopyNoWebPoMsg); l == nil || l["source"] != c.source {
				t.Errorf("no-copy line = %v, want one for the %s copy", l, c.source)
			}
		})
	}
	for _, tc := range []struct {
		source, dir string
		wantToken   string
	}{{"android_vr", "avr", ""}, {"visionos", "vos", ""}, {"tv_auth", "tv", "tok1"}} {
		t.Run("the re-fetch fails, "+tc.source, func(t *testing.T) {
			r := newFallbackRig(t)
			r.source = tc.source
			r.failFetch = true
			r.start(fallbackURL(tc.dir, 0, r.now.Add(5*time.Hour)), time.Time{}, false)
			if u, tok := r.refresh(); u != "" || tok != tc.wantToken {
				t.Errorf("refresh = (%q, %q), want (\"\", %q) — the current URL kept", u, tok, tc.wantToken)
			}
		})
	}
}

// webCreatorCopy is web_creator's copy of the itag-137 file: a members-only
// fallback's, which outranks the cookieless tier and carries its copy as a
// token-free shadow.
func webCreatorCopy(gen int, exp time.Time) youtube.Format {
	return withLevel(youtube.Format{Itag: 137, URL: fallbackURL("wc", gen, exp), ContentLength: "1000", Source: "web_creator"}, youtube.AuthLevelWebCreator)
}

// androidVRCopy is android_vr's copy of the itag-137 file.
func androidVRCopy(gen int, exp time.Time) youtube.Format {
	return withLevel(youtube.Format{Itag: 137, URL: fallbackURL("avr", gen, exp), ContentLength: "1000", Source: "android_vr"}, youtube.AuthLevelAndroidVR)
}

const noCopyNoWebPoMsg = "[POT] VOD URL refresh: no token-carrying copy of the same file — its token-free copy is from a client that takes no WebPO, so it is refreshed bare"

// TestVodTokenFallbackJudgesTheURLItHandsOut: whether a token may ride a URL
// is the question of the client that URL comes from, not of the client the
// stream was built from. A tv_auth stream's fresh token-free copy can be
// android_vr's — behind a web_creator winner once tv no longer returns the
// file, or from the cookieless chain — and the stream stays token-free
// across that hand-out, so the fallback's token must not ride it: step 1
// becomes the move onto the token-carrying copy, or, with none, the
// android_vr copy goes out bare as it always did. A refused android_vr URL
// then skips step 1 whatever client the stream was built from, and a failed
// re-fetch leaves no token on it. A tokenised stream — one whose tv_auth URLs
// took step 1's token — holds a token the engine sends on every URL, so it
// is never handed an android_vr URL at all: it moves onto the token-carrying
// copy, or keeps the URL it has, even when the fresh mint fails.
//
// Mutant: dropping the hand-out check from refreshVodURL — the web_creator
// row hands out the android_vr URL with tok1, the cookieless row hands out
// tokens on android_vr URLs, and the tokenised rows hand out the android_vr
// URL. Mutant: the check without its tokenised clause (only a fresh token
// counts) — the failed-mint row hands out the android_vr URL. Mutant: the
// tokenised branch dropped (no copy always means bare) — the no-copy
// tokenised row hands out the android_vr URL. Mutant: keep() and
// fallbackStep reading st.served.Source instead of st.urlSource — the
// cookieless row's refusals run step 1 and its failed re-fetch hands out a
// token. Mutant: handOut not recording st.urlSource — the same.
func TestVodTokenFallbackJudgesTheURLItHandsOut(t *testing.T) {
	t.Run("android_vr behind web_creator: onto the token-carrying copy", func(t *testing.T) {
		r := newFallbackRig(t)
		r.pool = func(gen int, exp time.Time) []youtube.Format {
			return youtube.MergeFormatPool(context.Background(), nil, []youtube.Format{webCreatorCopy(gen, exp), androidVRCopy(gen, exp)})
		}
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		r.expect(fallbackURL("wc", 1, r.now.Add(6*time.Hour)), "tok1", 2, "web_creator")
		if l := logLine(r.logs, "[POT] VOD URL refresh: the same file's token-free copy is from a client that takes no WebPO"); l == nil || l["source"] != "android_vr" {
			t.Errorf("move line = %v, want one naming the android_vr copy", l)
		}
	})
	t.Run("android_vr from the cookieless chain: bare", func(t *testing.T) {
		r := newFallbackRig(t)
		avr := fallbackURL("avr", 9, r.now.Add(6*time.Hour))
		fakeCookielessCtx(t, []youtube.Format{{Itag: 137, URL: avr, ContentLength: "1000", Source: "android_vr"}}, nil)
		r.pool = func(int, time.Time) []youtube.Format { return nil }
		r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
		r.expect(avr, "", 0, "android_vr")
		r.now = r.now.Add(time.Minute)
		r.expect(avr, "", 0, "android_vr")
		r.failFetch = true
		if u, tok := r.refresh(); u != "" || tok != "" {
			t.Errorf("refresh with the re-fetch failing = (%q, %q), want nothing — the current URL is android_vr's", u, tok)
		}
		if l := potLines(r.logs, noCopyNoWebPoMsg); len(l) != 2 || l[0]["source"] != "android_vr" {
			t.Errorf("no-copy lines = %v, want two naming the android_vr copy", l)
		}
		lines := r.fallbackLines()
		if len(lines) != 3 || lines[0]["step"] != 1 || lines[0]["source"] != "tv_auth" || lines[1]["step"] != 2 || lines[1]["source"] != "android_vr" || lines[2]["step"] != 2 {
			t.Errorf("fallback lines = %v, want step 1 for the tv_auth URL, then step 2 for each android_vr one", lines)
		}
	})
	for _, tc := range []struct {
		name      string
		pool      func(gen int, exp time.Time) []youtube.Format
		mintErr   error
		wantURL   func(r *fallbackRig) string
		wantToken string
	}{
		{"tokenised, android_vr behind web_creator: onto the token-carrying copy",
			func(gen int, exp time.Time) []youtube.Format {
				return youtube.MergeFormatPool(context.Background(), nil, []youtube.Format{webCreatorCopy(gen, exp), androidVRCopy(gen, exp)})
			}, nil, func(r *fallbackRig) string { return fallbackURL("wc", 2, r.now.Add(6*time.Hour)) }, "tok2"},
		{"tokenised, only android_vr: the current URL kept",
			func(gen int, exp time.Time) []youtube.Format { return []youtube.Format{androidVRCopy(gen, exp)} },
			nil, func(*fallbackRig) string { return "" }, "tok2"},
		{"tokenised, only android_vr, the mint failing: the current URL kept",
			func(gen int, exp time.Time) []youtube.Format { return []youtube.Format{androidVRCopy(gen, exp)} },
			errors.New("sidecar down"), func(*fallbackRig) string { return "" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFallbackRig(t)
			r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
			r.expect(fallbackURL("tv", 1, r.now.Add(6*time.Hour)), "tok1", 1, "tv_auth")
			// tv1's expiry, with tv gone from the fresh pool.
			r.now = r.now.Add(6*time.Hour - 15*time.Minute)
			r.pool, r.mintErr = tc.pool, tc.mintErr
			if u, tok := r.refresh(); u != tc.wantURL(r) || tok != tc.wantToken {
				t.Errorf("refresh = (%q, %q), want (%q, %q)", u, tok, tc.wantURL(r), tc.wantToken)
			}
		})
	}
}

// TestVodTokenFallbackStepsWaitOutTheirOwnAd: every URL a fallback step
// hands out comes from a player response with an ad of its own — step 1's
// from the fresh tv_auth response, step 2's copy from the watch page's — and
// is handed to the engine only once THAT ad is over, on the job's context
// rather than the refresh's bound, exactly as an ordinary refresh's URL is.
// Run on a synctest bubble's clock, so the waits are exact and take no time.
//
// Mutant: the wait dropped from refreshVodURL — step 1 returns at once.
// Mutant: step 2 waiting on the tv_auth winner's ad instead of its copy's —
// it returns after 5 s, not 12. Mutant: the wait on refreshCtx — the 12 s ad
// outlasts the 10 s budget at the 30 s MaximumTimeout floor, and step 2's URL
// never comes back.
func TestVodTokenFallbackStepsWaitOutTheirOwnAd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gen := 0
		stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
			gen++
			now := time.Now()
			tv := withLevel(youtube.Format{Itag: 137, URL: fallbackURL("tv", gen, now.Add(6*time.Hour)), ContentLength: "1000", Source: "tv_auth", AvailableAt: now.Add(5 * time.Second)}, youtube.AuthLevelTVAuth)
			wp := withLevel(youtube.Format{Itag: 137, URL: fallbackURL("web", gen, now.Add(6*time.Hour)), ContentLength: "1000", Source: "watch_page", AvailableAt: now.Add(12 * time.Second)}, youtube.AuthLevelWatchPageAuth)
			return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: fallbackPlayer, Formats: youtube.MergeFormatPool(context.Background(), nil, []youtube.Format{tv, wp})}
		})
		job, _ := vodPotJob(t)
		job.Config.MaximumTimeout = 30 // a 10 s refresh budget, shorter than the copy's 12 s ad
		job.YT = youtube.NewService(nil, &discardLogger{})
		minter := &fakePotProvider{generate: func(context.Context, string, bool) (string, error) { return "tok", nil }}
		servedURL := fallbackURL("tv", 0, time.Now().Add(5*time.Hour))
		served := withLevel(youtube.Format{Itag: 137, URL: servedURL, ContentLength: "1000", Source: "tv_auth"}, youtube.AuthLevelTVAuth)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: fallbackPlayer, Formats: []youtube.Format{served}}
		refresh := vodURLRefresh(context.Background(), job, info, served, servedURL, stubCipherSolver{}, nil, minter, "VOD video")
		for i, step := range []struct {
			dir string
			ad  time.Duration
		}{{"tv", 5 * time.Second}, {"web", 12 * time.Second}} {
			began := time.Now()
			u, tok := refresh()
			if took := time.Since(began); took != step.ad {
				t.Errorf("step %d returned after %v, want after its response's %v ad", i+1, took, step.ad)
			}
			if !strings.Contains(u, "/"+step.dir+"/") || tok != "tok" {
				t.Errorf("step %d = (%q, %q), want the %s URL with the token", i+1, u, tok, step.dir)
			}
		}
	})
}

// TestVodRefreshStateIsSafeForConcurrentRefreshes: the engine calls a
// stream's refresh from one goroutine, but nothing in its contract says so,
// and each step is decided on the one before. Two refusals answered at once
// are answered one after the other: one gets step 1, the other step 2.
//
// Mutant: dropping st.mu from refreshVodURL — -race reports the state's
// concurrent writes (CI's race flavour runs the whole module), and without
// -race the two refreshes can both read step 0 and both hand out step 1.
func TestVodRefreshStateIsSafeForConcurrentRefreshes(t *testing.T) {
	r := newFallbackRig(t)
	r.start(fallbackURL("tv", 0, r.now.Add(5*time.Hour)), time.Time{}, false)
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := range got {
		wg.Go(func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("refresh %d panicked: %v", i, p)
				}
			}()
			u, tok := r.refresh()
			got[i] = u + " " + tok
		})
	}
	wg.Wait()
	steps := strings.Join(got, "|")
	if !strings.Contains(steps, "/tv/") || !strings.Contains(steps, "/web/") {
		t.Errorf("concurrent refreshes = %q, want one step 1 (tv) and one step 2 (web)", steps)
	}
}

// vodTokenServer serves the itag-136 video and itag-140 audio files whole
// (2 chunks + 100 bytes each), answering a request accept refuses with 403
// — the way googlevideo refuses a URL it will not serve, at the very first
// request — and counting the refusals.
type vodTokenServer struct {
	*httptest.Server
	size    int64
	refused atomic.Int32
}

func newVodTokenServer(t *testing.T, accept func(r *http.Request) bool) *vodTokenServer {
	t.Helper()
	s := &vodTokenServer{size: 2*engine.DownloadChunkSize + 100}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accept(r) {
			s.refused.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end <= 0 || end >= s.size {
			end = s.size - 1
		}
		body := make([]byte, end-start+1)
		if start == 0 {
			copy(body, "\x00\x00\x00\x18ftypdash")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, s.size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// pool is generation gen of the server's two files, deduplicated for real:
// tv_auth copies winning, each carrying the watch page's copy of the same
// file as its TokenAlternate. Every URL expires six hours after base.
func (s *vodTokenServer) pool(gen int, base time.Time) []youtube.Format {
	w, h, fps := 1280, 720, 30
	clen := strconv.FormatInt(s.size, 10)
	url := func(dir, itag string) string {
		return fmt.Sprintf("%s/%s/g%d/videoplayback?expire=%d&id=o-%s%d%s&itag=%s&clen=%s", s.URL, dir, gen, base.Add(6*time.Hour).Unix(), dir, gen, itag, itag, clen)
	}
	var copies []youtube.Format
	for _, c := range []struct {
		dir, source string
		level       int
	}{{"tv", "tv_auth", youtube.AuthLevelTVAuth}, {"web", "watch_page", youtube.AuthLevelWatchPageAuth}} {
		copies = append(copies,
			withLevel(youtube.Format{Itag: 136, URL: url(c.dir, "136"), MimeType: `video/mp4; codecs="avc1.4d401f"`, Bitrate: 1_500_000, Width: &w, Height: &h, Fps: &fps, ContentLength: clen, Source: c.source}, c.level),
			withLevel(youtube.Format{Itag: 140, URL: url(c.dir, "140"), MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128_000, ContentLength: clen, Source: c.source}, c.level))
	}
	return youtube.MergeFormatPool(context.Background(), nil, copies)
}

// TestVodTokenFallbackRescuesTheFieldCase drives the field report end to end
// through DownloadVod, the real engine and an origin playing YouTube's part:
// a signed-in account without Premium, tv_auth winning both streams, and
// every tv_auth URL refused at its first request — the size probe — bare or
// with a token, while the watch page's copy of the same file is served with
// one. The probe's two refreshes are the fallback's two steps (a fresh
// tv_auth URL with a token, refused too; then the watch page's copy with a
// token), and the download completes where the field log ended on "size
// probe refused: HTTP 403".
//
// The second row pins that an expiry is left alone end to end, and that the
// URL DownloadVod resolved is the one the rule reads: setup URLs ten minutes
// from their expiry, refused, and fresh tv_auth URLs served bare. The
// download completes on a bare tv_auth URL and no token is ever attached.
//
// No response here carries a pre-roll ad: the httptest origin runs outside
// any synctest bubble, and the engine's HTTP client has no seam a worker
// test could put one on. How the fallback composes with the ad wait is
// pinned a level down, on the closure itself —
// TestVodTokenFallbackStepsWaitOutTheirOwnAd and
// TestVodTokenFallbackSparesTheAdWindow — beside DownloadVod's own wait
// (TestDownloadVodWaitsOutThePreRollAd).
//
// Mutant: dropping the fallback from refreshVodURL — the field row fails on
// "size probe refused: HTTP 403". Mutant: DownloadVod handing vodURLRefresh
// "" (or the raw format URL's absence) instead of videoResolved / audioResolved
// — the expiry row reads the setup URL's age instead of its expiry, runs step
// 1 and attaches a token.
func TestVodTokenFallbackRescuesTheFieldCase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setupLife time.Duration // how long the setup URLs have left
		accept    func(r *http.Request) bool
		wantToken string
		wantSteps []int // the closing lines' steps, per stream
	}{
		{"every tv_auth URL refused, the watch page's served with a token", 5 * time.Hour,
			func(r *http.Request) bool {
				return strings.Contains(r.URL.Path, "/web/") && r.URL.Query().Get("pot") == "tok"
			}, "tok", []int{1, 2}},
		{"an expired setup URL, fresh tv_auth URLs served bare", 10 * time.Minute,
			func(r *http.Request) bool {
				return !strings.Contains(r.URL.Path, "/g0/") && r.URL.Query().Get("pot") == ""
			}, "", []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newVodTokenServer(t, tc.accept)
			base := time.Unix(1_800_000_000, 0)
			origNow, origMinter := vodRefreshNow, vodRefreshMinter
			vodRefreshNow = func() time.Time { return base }
			var mints atomic.Int32
			vodRefreshMinter = func(*bgutils.PotProvider) gvsTokenMinter {
				return &fakePotProvider{generate: func(context.Context, string, bool) (string, error) {
					mints.Add(1)
					return "tok", nil
				}}
			}
			t.Cleanup(func() { vodRefreshNow, vodRefreshMinter = origNow, origMinter })
			var gen atomic.Int32
			stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
				return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: fallbackPlayer, Formats: srv.pool(int(gen.Add(1)), base)}
			})

			job, logs := vodPotJob(t)
			job.YT = youtube.NewService(nil, &discardLogger{})
			setup := srv.pool(0, base.Add(tc.setupLife-6*time.Hour))
			info := &youtube.VideoInfo{PlayerURL: fallbackPlayer, StreamStatus: youtube.StreamVOD, Formats: setup}
			res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, nil, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			if res.VideoFormat.Source != "tv_auth" || res.AudioFormat.Source != "tv_auth" {
				t.Fatalf("setup served %s / %s, want tv_auth for both", res.VideoFormat.Source, res.AudioFormat.Source)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			o := &DownloadOrchestrator{logger: discardLogger{}}
			if err := o.runDownloaders(ctx, res); err != nil {
				t.Fatalf("download: %v", err)
			}
			for _, p := range []string{res.VideoPath, res.AudioPath} {
				if fi, err := os.Stat(p); err != nil || fi.Size() != srv.size {
					t.Errorf("%s = %v (err %v), want the whole %d-byte file", p, fi, err, srv.size)
				}
			}
			for _, d := range []*engine.SegmentDownloader{res.VideoDownloader, res.AudioDownloader} {
				if got := d.PoToken(); got != tc.wantToken {
					t.Errorf("downloader token = %q, want %q", got, tc.wantToken)
				}
			}
			for _, tag := range []string{"VOD video", "VOD audio"} {
				var steps []int
				for _, l := range potLines(logs, "[POT] VOD URL refresh") {
					if l["tag"] == tag {
						steps = append(steps, l["step"].(int))
					}
				}
				if fmt.Sprint(steps) != fmt.Sprint(tc.wantSteps) {
					t.Errorf("%s refresh steps = %v, want %v", tag, steps, tc.wantSteps)
				}
			}
			if tc.wantToken == "" && mints.Load() != 0 {
				t.Errorf("minted %d tokens for an expiry", mints.Load())
			}
		})
	}
}

// TestVodRefreshMinterIsTheJobsProvider pins the seam's production value:
// the job's own provider, unchanged — every whole-file refresh mint goes
// through it, the token fallback's and a token-requiring stream's re-mint
// alike, and a seam that lost it would silently refresh every stream bare —
// and a disabled (nil) provider as a minter minterUsable turns away.
//
// Mutant: the production value returning nil — the first check.
func TestVodRefreshMinterIsTheJobsProvider(t *testing.T) {
	p := &bgutils.PotProvider{}
	if got := vodRefreshMinter(p); got != gvsTokenMinter(p) {
		t.Errorf("vodRefreshMinter(p) = %#v, want the provider itself", got)
	}
	if m := vodRefreshMinter(nil); minterUsable(m) {
		t.Errorf("vodRefreshMinter(nil) = %#v, which minterUsable accepts — a disabled provider must read as none", m)
	}
}

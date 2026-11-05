package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestRefreshVodURLRemintsForAMovedVisitorSession pins owner decision 4 of
// wave FC: a whole-file refresh whose fresh extraction ran under another
// visitor session than the one the stream's token is bound to re-mints the
// token under the fresh response's binding (bypassCache) and hands URL and
// token out together — yt-dlp's per-response pairing, each extraction
// minting under its own visitor_data (_video.py:3084-3096, 1953-1960). The
// stream pairs with the fresh binding from then on, so a later refresh under
// that session mints once, under it. A session that did not move, a fresh
// binding that is no visitor data, and a hand-out no token rides mint as
// before; a re-mint that fails keeps the current URL with its own session's
// token. The re-mint runs before the fresh response's pre-roll wait, and a
// cancel during that wait keeps the current URL with its own token. A
// guessed fresh binding (signed in, the fresh page stated no datasync ID)
// never replaces a stated one: the account did not move, and the token
// minted under the stated binding rides the fresh URL; a guessed pair moves
// to another guess like any visitor session.
//
// Mutants (each run): dropping the re-mint fails the moved row (the
// setup-bound token on the fresh URL); re-minting without bypassCache fails
// its bypass check; not recording the pair's binding fails the second
// refresh (it mints under vd-A again and re-mints, four mints); re-minting
// whatever the binding kind fails the datasync row; re-minting for a
// hand-out no token rides fails the bare row (a mint); handing out the
// fresh URL on a failed re-mint fails its row; not restoring the current
// token on the wait's cancel fails the cancel row (the fresh session's token
// on the old URL); letting a guessed fresh binding replace a stated one
// fails the two no-move-to-a-guess rows (wave FC review); refusing every
// guessed fresh binding fails the guessed-pair row; not seeding the setup's
// guess (vodURLRefresh) fails TestVodURLRefreshSeedsTheSetupGuess; not
// recording the fresh pair's guess on a move fails the stays-stated subtest.
func TestRefreshVodURLRemintsForAMovedVisitorSession(t *testing.T) {
	const player = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	const old, freshURL = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000"
	type mint struct {
		binding string
		bypass  bool
	}
	type setup struct {
		binding string
		guessed bool
	}
	visitor := setup{"vd-A", false}
	run := func(t *testing.T, ctx context.Context, su setup, source, freshKind, freshBinding string, freshGuessed bool, mintErr error, availableAt time.Time) (func() (string, string), *[]mint) {
		t.Helper()
		stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
			return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: player,
				GvsBinding: freshBinding, GvsBindingKind: freshKind, GvsBindingGuessed: freshGuessed,
				Formats: []youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: source, AvailableAt: availableAt}}}
		})
		var mu sync.Mutex
		var mints []mint
		minter := &fakePotProvider{generate: func(_ context.Context, binding string, bypass bool) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			mints = append(mints, mint{binding, bypass})
			if mintErr != nil && binding == freshBinding && binding != su.binding {
				return "", mintErr
			}
			return "tok-" + binding, nil
		}}
		job, _ := vodPotJob(t)
		job.YT = youtube.NewService(nil, &discardLogger{})
		st := newVodRefreshState(nil, youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: source}, old)
		st.bindingGuessed = su.guessed
		refresh := func() (string, string) {
			return refreshVodURL(ctx, job, player, st, stubCipherSolver{}, nil, minter, su.binding, "VOD video")
		}
		return refresh, &mints
	}

	for _, tc := range []struct {
		name         string
		setup        *setup
		source       string
		freshKind    string
		freshBinding string
		freshGuessed bool
		mintErr      error
		wantURL      string
		wantToken    string
		wantMints    []mint
	}{
		{name: "the session moved: re-minted for the fresh one", source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B",
			wantURL: freshURL, wantToken: "tok-vd-B", wantMints: []mint{{"vd-A", true}, {"vd-B", true}}},
		{name: "the same session: one mint", source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-A",
			wantURL: freshURL, wantToken: "tok-vd-A", wantMints: []mint{{"vd-A", true}}},
		{name: "a datasync binding is the same on every extraction", source: "web_creator", freshKind: youtube.BindingDataSyncID, freshBinding: "ds",
			wantURL: freshURL, wantToken: "tok-vd-A", wantMints: []mint{{"vd-A", true}}},
		{name: "no token rides a bare hand-out", source: "tv_auth", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B",
			wantURL: freshURL},
		// A setup bound to the account's datasync ID whose re-extraction lost
		// it: the fresh binding is guessed visitor data, the account did not
		// move, and the datasync-bound token rides the fresh URL as it is.
		{name: "a datasync pair does not move to a guessed fresh binding", setup: &setup{"ds-account", false},
			source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B", freshGuessed: true,
			wantURL: freshURL, wantToken: "tok-ds-account", wantMints: []mint{{"ds-account", true}}},
		// A stated visitor-data pair is not replaced by a guess either.
		{name: "a stated visitor pair does not move to a guess", source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B", freshGuessed: true,
			wantURL: freshURL, wantToken: "tok-vd-A", wantMints: []mint{{"vd-A", true}}},
		// A guessed pair moves with its session like any visitor pair.
		{name: "a guessed pair moves to the fresh guess", setup: &setup{"vd-A", true},
			source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B", freshGuessed: true,
			wantURL: freshURL, wantToken: "tok-vd-B", wantMints: []mint{{"vd-A", true}, {"vd-B", true}}},
		{name: "a failed re-mint keeps the current URL", source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B", mintErr: errors.New("sidecar down"),
			wantToken: "tok-vd-A", wantMints: []mint{{"vd-A", true}, {"vd-B", true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			su := visitor
			if tc.setup != nil {
				su = *tc.setup
			}
			refresh, mints := run(t, context.Background(), su, tc.source, tc.freshKind, tc.freshBinding, tc.freshGuessed, tc.mintErr, time.Time{})
			gotURL, gotToken := refresh()
			if gotURL != tc.wantURL || gotToken != tc.wantToken {
				t.Errorf("refreshVodURL = (%q, %q), want (%q, %q)", gotURL, gotToken, tc.wantURL, tc.wantToken)
			}
			if len(*mints) != len(tc.wantMints) {
				t.Fatalf("mints = %v, want %v", *mints, tc.wantMints)
			}
			for i := range tc.wantMints {
				if (*mints)[i] != tc.wantMints[i] {
					t.Errorf("mint %d = %+v, want %+v", i, (*mints)[i], tc.wantMints[i])
				}
			}
		})
	}

	t.Run("the stream pairs with the fresh session from then on", func(t *testing.T) {
		refresh, mints := run(t, context.Background(), visitor, "web_creator", youtube.BindingVisitorData, "vd-B", false, nil, time.Time{})
		refresh()
		gotURL, gotToken := refresh()
		if gotURL != freshURL || gotToken != "tok-vd-B" {
			t.Errorf("second refresh = (%q, %q), want the fresh URL with tok-vd-B", gotURL, gotToken)
		}
		if want := []mint{{"vd-A", true}, {"vd-B", true}, {"vd-B", true}}; len(*mints) != len(want) || (*mints)[2] != want[2] {
			t.Errorf("mints = %v, want %v — the second refresh mints once, under the session it pairs with", *mints, want)
		}
	})

	// A guessed pair that moved onto a stated session is a stated pair from
	// then on: a later guessed fresh binding does not move it again.
	t.Run("a pair that moved onto a stated session stays stated", func(t *testing.T) {
		fresh := []*youtube.VideoInfo{
			{GvsBinding: "vd-B", GvsBindingKind: youtube.BindingVisitorData},
			{GvsBinding: "vd-C", GvsBindingKind: youtube.BindingVisitorData, GvsBindingGuessed: true},
		}
		var n int
		stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
			info := *fresh[n]
			n++
			info.StreamStatus, info.PlayerURL = youtube.StreamVOD, player
			info.Formats = []youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "web_creator"}}
			return &info
		})
		var mints []string
		minter := &fakePotProvider{generate: func(_ context.Context, binding string, _ bool) (string, error) {
			mints = append(mints, binding)
			return "tok-" + binding, nil
		}}
		job, _ := vodPotJob(t)
		job.YT = youtube.NewService(nil, &discardLogger{})
		st := newVodRefreshState(nil, youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "web_creator"}, old)
		st.bindingGuessed = true
		refreshVodURL(context.Background(), job, player, st, stubCipherSolver{}, nil, minter, "vd-A", "VOD video")
		if _, tok := refreshVodURL(context.Background(), job, player, st, stubCipherSolver{}, nil, minter, "vd-A", "VOD video"); tok != "tok-vd-B" {
			t.Errorf("second refresh token = %q, want tok-vd-B (mints %v) — a guess does not replace the stated pair", tok, mints)
		}
	})

	// The re-mint composes with the fresh response's pre-roll wait: the pair
	// goes out once the ad is over, and a cancel during the wait keeps the
	// current URL with its own session's token, never the fresh one's.
	t.Run("the pair waits out the fresh ad", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			refresh, _ := run(t, context.Background(), visitor, "web_creator", youtube.BindingVisitorData, "vd-B", false, nil, time.Now().Add(5*time.Second))
			type got struct{ url, tok string }
			out := make(chan got, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("refreshVodURL panicked: %v", p)
					}
				}()
				u, tok := refresh()
				out <- got{u, tok}
			}()
			time.Sleep(5*time.Second - time.Millisecond)
			synctest.Wait()
			select {
			case g := <-out:
				t.Fatalf("refreshVodURL = %v before the fresh ad was over", g)
			default:
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			select {
			case g := <-out:
				if g.url != freshURL || g.tok != "tok-vd-B" {
					t.Errorf("refreshVodURL = %v, want the fresh URL with tok-vd-B", g)
				}
			default:
				t.Fatal("refreshVodURL still waiting once the ad was over")
			}
		})
	})

	t.Run("a cancel during the wait keeps the current URL and its token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refresh, _ := run(t, ctx, visitor, "web_creator", youtube.BindingVisitorData, "vd-B", false, nil, time.Now().Add(5*time.Second))
			type got struct{ url, tok string }
			out := make(chan got, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("refreshVodURL panicked: %v", p)
					}
				}()
				u, tok := refresh()
				out <- got{u, tok}
			}()
			time.Sleep(2 * time.Second)
			synctest.Wait()
			cancel()
			synctest.Wait()
			select {
			case g := <-out:
				if g.url != "" || g.tok != "tok-vd-A" {
					t.Errorf("refreshVodURL = %v after a cancel, want the current URL kept with tok-vd-A", g)
				}
			default:
				t.Fatal("refreshVodURL still waiting after the cancel")
			}
		})
	})
}

// TestVodURLRefreshSeedsTheSetupGuess pins the production seeding of the
// refresh state's bindingGuessed (vodURLRefresh): a stream set up under a
// guessed visitor-data binding moves to the fresh extraction's guess, as
// TestRefreshVodURLRemintsForAMovedVisitorSession's guessed-pair row does on
// a hand-seeded state. Unseeded, the setup reads as a stated binding and the
// guessed fresh binding is refused (the setup's token on the fresh URL).
func TestVodURLRefreshSeedsTheSetupGuess(t *testing.T) {
	const player = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	const old, freshURL = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000"
	stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: player,
			GvsBinding: "vd-B", GvsBindingKind: youtube.BindingVisitorData, GvsBindingGuessed: true,
			Formats: []youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "web_creator"}}}
	})
	minter := &fakePotProvider{generate: func(_ context.Context, binding string, _ bool) (string, error) {
		return "tok-" + binding, nil
	}}
	job, _ := vodPotJob(t)
	job.YT = youtube.NewService(nil, &discardLogger{})
	setup := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: player,
		GvsBinding: "vd-A", GvsBindingKind: youtube.BindingVisitorData, GvsBindingGuessed: true}
	served := youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "web_creator"}
	refresh := vodURLRefresh(context.Background(), job, setup, served, old, stubCipherSolver{}, nil, minter, "VOD video")
	if gotURL, gotToken := refresh(); gotURL != freshURL || gotToken != "tok-vd-B" {
		t.Errorf("refresh = (%q, %q), want the fresh URL with tok-vd-B — the setup's guess seeds the state", gotURL, gotToken)
	}
}

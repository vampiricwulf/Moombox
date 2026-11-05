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
// cancel during that wait keeps the current URL with its own token.
//
// Mutants (each run): dropping the re-mint fails the moved row (the
// setup-bound token on the fresh URL); re-minting without bypassCache fails
// its bypass check; not recording the pair's binding fails the second
// refresh (it mints under vd-A again and re-mints, four mints); re-minting
// whatever the binding kind fails the datasync row; re-minting for a
// hand-out no token rides fails the bare row (a mint); handing out the
// fresh URL on a failed re-mint fails its row; not restoring the current
// token on the wait's cancel fails the cancel row (the fresh session's token
// on the old URL).
func TestRefreshVodURLRemintsForAMovedVisitorSession(t *testing.T) {
	const player = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	const old, freshURL = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000"
	type mint struct {
		binding string
		bypass  bool
	}
	run := func(t *testing.T, ctx context.Context, source, freshKind, freshBinding string, mintErr error, availableAt time.Time) (func() (string, string), *[]mint) {
		t.Helper()
		stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
			return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: player,
				GvsBinding: freshBinding, GvsBindingKind: freshKind,
				Formats: []youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: source, AvailableAt: availableAt}}}
		})
		var mu sync.Mutex
		var mints []mint
		minter := &fakePotProvider{generate: func(_ context.Context, binding string, bypass bool) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			mints = append(mints, mint{binding, bypass})
			if mintErr != nil && binding == freshBinding && binding != "vd-A" {
				return "", mintErr
			}
			return "tok-" + binding, nil
		}}
		job, _ := vodPotJob(t)
		job.YT = youtube.NewService(nil, &discardLogger{})
		st := newVodRefreshState(nil, youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: source}, old)
		refresh := func() (string, string) {
			return refreshVodURL(ctx, job, player, st, stubCipherSolver{}, nil, minter, "vd-A", "VOD video")
		}
		return refresh, &mints
	}

	for _, tc := range []struct {
		name         string
		source       string
		freshKind    string
		freshBinding string
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
		{name: "a failed re-mint keeps the current URL", source: "web_creator", freshKind: youtube.BindingVisitorData, freshBinding: "vd-B", mintErr: errors.New("sidecar down"),
			wantToken: "tok-vd-A", wantMints: []mint{{"vd-A", true}, {"vd-B", true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refresh, mints := run(t, context.Background(), tc.source, tc.freshKind, tc.freshBinding, tc.mintErr, time.Time{})
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
		refresh, mints := run(t, context.Background(), "web_creator", youtube.BindingVisitorData, "vd-B", nil, time.Time{})
		refresh()
		gotURL, gotToken := refresh()
		if gotURL != freshURL || gotToken != "tok-vd-B" {
			t.Errorf("second refresh = (%q, %q), want the fresh URL with tok-vd-B", gotURL, gotToken)
		}
		if want := []mint{{"vd-A", true}, {"vd-B", true}, {"vd-B", true}}; len(*mints) != len(want) || (*mints)[2] != want[2] {
			t.Errorf("mints = %v, want %v — the second refresh mints once, under the session it pairs with", *mints, want)
		}
	})

	// The re-mint composes with the fresh response's pre-roll wait: the pair
	// goes out once the ad is over, and a cancel during the wait keeps the
	// current URL with its own session's token, never the fresh one's.
	t.Run("the pair waits out the fresh ad", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			refresh, _ := run(t, context.Background(), "web_creator", youtube.BindingVisitorData, "vd-B", nil, time.Now().Add(5*time.Second))
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
			refresh, _ := run(t, ctx, "web_creator", youtube.BindingVisitorData, "vd-B", nil, time.Now().Add(5*time.Second))
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

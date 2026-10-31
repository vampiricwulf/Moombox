package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// challengeSolver is a routed cipher solver whose signature answer is sigErr
// (or "SIGNED") and whose n answer is n(challenge), counting every question.
type challengeSolver struct {
	sigErr error
	n      func(challenge string) string
	asked  atomic.Int32
}

func (s *challengeSolver) Sig(context.Context, string, string) (string, error) {
	s.asked.Add(1)
	if s.sigErr != nil {
		return "", s.sigErr
	}
	return "SIGNED", nil
}

func (s *challengeSolver) N(_ context.Context, _, c string) (string, error) {
	s.asked.Add(1)
	if s.n == nil {
		return nSolved, nil
	}
	return s.n(c), nil
}

func (s *challengeSolver) Batch(context.Context, string, []string, []string) (map[string]string, map[string]string, error) {
	return nil, nil, errors.New("challengeSolver: Batch not used")
}

// tvChallengeInfo is a VOD pool as the public cascade leaves it when TV was
// adequate: tv_public's 302 video and 251 audio, the only copies, each
// needing the player JS — an n in its URL, and with signed a signatureCipher
// signature too — and no cookieless copy at all (the cascade asked no
// cookieless client).
func tvChallengeInfo(signed bool) *youtube.VideoInfo {
	info := vodPotInfo("tv_public", "tv_public")
	for i := range info.Formats {
		f := &info.Formats[i]
		f.URL += "&n=" + nChal
		if signed {
			f.EncryptedSig = "ENCRYPTEDSIG"
		}
		*f = withLevel(*f, youtube.AuthLevelTVPublic)
	}
	return info
}

// visionosCopies are the cookieless chain's copies of info's formats: same
// streams, URLs with no n and no signature.
func visionosCopies(info *youtube.VideoInfo) []youtube.Format {
	out := make([]youtube.Format, 0, len(info.Formats))
	for _, f := range info.Formats {
		c := cookielessCopy(f)
		c.URL, c.EncryptedSig = strings.TrimSuffix(c.URL, "&n="+nChal), ""
		out = append(out, c)
	}
	return out
}

// TestDownloadVodFallsBackToChallengeFreeFormats is audit G10's VOD half.
// When the chosen format's URL fails to resolve for a reason every format
// needing the player JS shares — the sidecar down (cipher.ErrSidecarUnavailable),
// a player ejs cannot solve (cipher.ErrPlayerJSStale), an n no solver
// answered (cipher.ErrNUnsolved), no player URL — the job used to re-select
// once with that one itag excluded, fail again on the next tv format, and end
// in Error ("video URL resolve failed for primary and alternate"), where
// yt-dlp downloads visionos's copy: it skips each format whose challenge
// fails before recording the stream (_video.py:3509-3511, 3557-3580,
// 3587-3589). Now every challenge-bearing format is dropped, the cookieless
// chain is asked once, its copies are merged (MergeFormatPool) and the
// selection runs again: each systemic row downloads visionos's 302 and 251,
// from their own URLs, the solver asked about the first failure only.
//
// A resolve failure that is not systemic (a sidecar that is up failing one
// request) keeps the single-itag re-selection and asks no cookieless client.
// A failed re-extract with nothing left is a clear error carrying the cause.
// One re-extract serves both degrades: a missing_pot degrade after the
// challenge-free one reuses its answer.
//
// Mutants: resolveVodURLs' systemic early return dropped (each systemic row
// asks the solver twice, re-selecting); DownloadVod's errChallengesUnsolvable
// branch dropped (each systemic row fails); the cookieless re-extract skipped
// (each systemic row fails: nothing is left); withoutChallengeFormats keeping
// challenge-bearing copies (the tv formats win again and fail); the missing
// player URL dropped from systemicResolveFailure (that row fails "primary and
// alternate"); the missing_pot block calling fetchCookielessFormats itself
// (the shared row re-extracts twice); the missing_pot merge taking the
// re-extract's challenge-bearing copies after a challenge-free degrade (the
// shared row selects android_vr's audio and fails to resolve it); the drop
// line demoted from Warn (each systemic row).
func TestDownloadVodFallsBackToChallengeFreeFormats(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signed    bool
		noPlayer  bool
		solver    *challengeSolver
		wantAsked int32
		cause     error
	}{
		{"sidecar down", true, false, &challengeSolver{sigErr: fmt.Errorf("%w: the sidecar is not running", cipher.ErrSidecarUnavailable)}, 1, cipher.ErrSidecarUnavailable},
		{"player JS stale", true, false, &challengeSolver{sigErr: fmt.Errorf("%w: ejs solve sig: no solutions", cipher.ErrPlayerJSStale)}, 1, cipher.ErrPlayerJSStale},
		{"n unsolved", false, false, &challengeSolver{n: sentinelOf}, 1, cipher.ErrNUnsolved},
		{"no player URL", true, true, &challengeSolver{}, 0, errChallengesUnsolvable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := tvChallengeInfo(tc.signed)
			if tc.noPlayer {
				info.PlayerURL = ""
			}
			calls := fakeCookieless(t, visionosCopies(info), nil)
			built := recordDownloaders(t)
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			res, err := DownloadVod(context.Background(), job, info, tc.solver, nil, nil, nil)
			if err != nil {
				t.Fatalf("DownloadVod = %v, want the visionos copies", err)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("cookieless re-extract ran %d times, want 1", n)
			}
			if n := tc.solver.asked.Load(); n != tc.wantAsked {
				t.Errorf("solver asked %d times, want %d: one failure, then formats that need nothing solved", n, tc.wantAsked)
			}
			if v := res.VideoFormat; v == nil || v.Itag != 302 || v.Source != "visionos" {
				t.Errorf("video format = %+v, want visionos 302", v)
			}
			if a := res.AudioFormat; a == nil || a.Itag != 251 || a.Source != "visionos" {
				t.Errorf("audio format = %+v, want visionos 251", a)
			}
			if len(*built) != 2 || (*built)[0].BaseURL != "http://127.0.0.1:1/visionos/videoplayback?itag=302" ||
				(*built)[1].BaseURL != "http://127.0.0.1:1/visionos/videoplayback?itag=251" {
				t.Errorf("downloaders built on %+v, want the visionos URLs as sent", *built)
			}
			drop := logLine(logs, "[Cipher] VOD: the player's JS challenges cannot be solved")
			if drop == nil || drop["dropped"] != 2 {
				t.Errorf("drop line = %v, want dropped=2", drop)
			}
			if cause, _ := drop["err"].(error); !errors.Is(cause, tc.cause) {
				t.Errorf("drop line's err = %v, want one matching %v", drop["err"], tc.cause)
			}
			if !rec.warned("[Cipher] VOD: the player's JS challenges cannot be solved") {
				t.Error("the drop line is not a Warn")
			}
			if logLine(logs, "[Cipher] VOD: serving formats that need no player JS") == nil {
				t.Errorf("no serving line: %v", logs.lines())
			}
		})
	}

	t.Run("a failure that is not systemic re-selects as before", func(t *testing.T) {
		info := tvChallengeInfo(true)
		calls := fakeCookieless(t, visionosCopies(info), nil)
		job, _ := reextractJob(t)
		solver := &challengeSolver{sigErr: errors.New("context deadline exceeded")}
		_, err := DownloadVod(context.Background(), job, info, solver, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "primary and alternate") || errors.Is(err, errChallengesUnsolvable) {
			t.Errorf("DownloadVod = %v, want the re-selection's own failure, unmarked", err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("cookieless re-extract ran %d times, want 0", n)
		}
	})

	t.Run("a failed re-extract with nothing left is a clear error", func(t *testing.T) {
		calls := fakeCookieless(t, nil, errors.New("visionos 403"))
		job, logs := reextractJob(t)
		solver := &challengeSolver{sigErr: fmt.Errorf("%w: the sidecar is not running", cipher.ErrSidecarUnavailable)}
		_, err := DownloadVod(context.Background(), job, tvChallengeInfo(true), solver, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "without solving the player JS") || !errors.Is(err, cipher.ErrSidecarUnavailable) {
			t.Errorf("DownloadVod = %v, want the no-format error carrying the cause", err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless re-extract ran %d times, want 1", n)
		}
		if logLine(logs, "[Cipher] VOD: cookieless re-extract failed") == nil {
			t.Errorf("no re-extract failure line: %v", logs.lines())
		}
	})

	t.Run("one re-extract serves the missing_pot degrade too", func(t *testing.T) {
		fakeVodMint(t, "", errors.New("sidecar down"))
		info := tvChallengeInfo(false)
		// web_creator's copies need nothing solved but a GVS token none can
		// be minted: the tv video's TokenAlternate and the audio survive the
		// challenge drop, then leave with missing_pot — the video for the
		// visionos shadow the re-dedup gives it, the audio lost.
		wc := func(f youtube.Format) youtube.Format {
			f = withLevel(f, youtube.AuthLevelWebCreator)
			f.Source, f.URL = "web_creator", strings.TrimSuffix(f.URL, "&n="+nChal)
			return f
		}
		alt := wc(info.Formats[0])
		info.Formats[0].TokenAlternate = &alt
		// The cookieless answer also holds an android_vr audio whose URL
		// carries an n: neither degrade may take it.
		vrAudio := withLevel(info.Formats[1], youtube.AuthLevelAndroidVR)
		vrAudio.Source, vrAudio.URL = "android_vr", "http://127.0.0.1:1/vr/videoplayback?itag=251&n="+nChal
		info.Formats[1] = wc(info.Formats[1])
		calls := fakeCookieless(t, append(visionosCopies(info)[:1], vrAudio), nil)
		job, logs := reextractJob(t)
		res, err := DownloadVod(context.Background(), job, info, &challengeSolver{n: sentinelOf}, nil, &bgutils.PotProvider{}, nil)
		if err != nil {
			t.Fatalf("DownloadVod = %v, want the visionos video", err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless re-extract ran %d times, want 1 across both degrades", n)
		}
		if logLine(logs, "[POT] missing_pot: cookieless re-extract merged") == nil {
			t.Errorf("the missing_pot degrade did not merge the shared re-extract: %v", logs.lines())
		}
		if v := res.VideoFormat; v == nil || v.Itag != 302 || v.Source != "visionos" {
			t.Errorf("video format = %+v, want visionos 302, the web_creator copy's shadow", v)
		}
	})
}

// TestSystemicResolveFailure pins which resolve failures are systemic: the
// cipher's three sentinels and a missing player URL, for a format that needs
// the player JS, and nothing for one that does not — its URL needs nothing
// any of them is about.
//
// Mutants: any sentinel dropped (its row); the NeedsPlayerJS gate dropped
// (the direct-URL rows); the empty-player arm dropped (its row).
func TestSystemicResolveFailure(t *testing.T) {
	challenged := &youtube.Format{Itag: 1, URL: "https://x/videoplayback?n=" + nChal}
	direct := &youtube.Format{Itag: 2, URL: "https://x/videoplayback?itag=2"}
	plain := errors.New("context deadline exceeded")
	for _, tc := range []struct {
		name   string
		err    error
		f      *youtube.Format
		player string
		want   bool
	}{
		{"sidecar unavailable", fmt.Errorf("x: %w", cipher.ErrSidecarUnavailable), challenged, nPlayer, true},
		{"player JS stale", fmt.Errorf("x: %w", cipher.ErrPlayerJSStale), challenged, nPlayer, true},
		{"n unsolved", fmt.Errorf("x: %w", cipher.ErrNUnsolved), challenged, nPlayer, true},
		{"no player URL", plain, challenged, "", true},
		{"a request's own failure", plain, challenged, nPlayer, false},
		{"a direct URL, sentinel", fmt.Errorf("x: %w", cipher.ErrNUnsolved), direct, nPlayer, false},
		{"a direct URL, no player", plain, direct, "", false},
		{"no error", nil, challenged, "", false},
	} {
		if got := systemicResolveFailure(tc.err, tc.f, tc.player); got != tc.want {
			t.Errorf("%s: systemicResolveFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNoSolverWiredIsTheSidecarUnavailable: a signatureCipher format resolved
// with no solver wired at all — neither the routed composite nor goja — fails
// with cipher.ErrSidecarUnavailable wrapped, so it is the systemic failure it
// is (the sidecar is the only signature solver) and the setup falls back to
// the formats that need no player JS. It used to be an untyped "requires sig
// but no solver wired", which the fallback could not tell from one URL's own
// failure.
//
// Mutants: either wrap reverted to the untyped error (its row).
func TestNoSolverWiredIsTheSidecarUnavailable(t *testing.T) {
	signed := youtube.Format{Itag: 299, URL: "https://x/videoplayback?itag=299", EncryptedSig: "ENCSIG", SigKey: "sig"}
	logs := &captureLogger{}
	for _, tc := range []struct {
		name    string
		resolve func() (string, error)
	}{
		{"resolveFormatURL", func() (string, error) {
			return resolveFormatURL(context.Background(), &signed, nil, nil, nPlayer, logs)
		}},
		{"resolveFormatURLByItag", func() (string, error) {
			return resolveFormatURLByItag(context.Background(), []youtube.Format{signed}, 299, nil, nil, nPlayer, logs)
		}},
	} {
		_, err := tc.resolve()
		if !errors.Is(err, cipher.ErrSidecarUnavailable) {
			t.Errorf("%s = %v, want cipher.ErrSidecarUnavailable wrapped", tc.name, err)
		}
		if !systemicResolveFailure(err, &signed, nPlayer) {
			t.Errorf("%s: %v is not a systemic resolve failure", tc.name, err)
		}
	}
}

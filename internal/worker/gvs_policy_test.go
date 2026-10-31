package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The GVS token policy's per-stream and per-session halves (audit wave W10):
// a token is attached wherever the client takes one and one was minted
// (youtube.GvsTokenRequired), but only a stream whose token is mandatory
// (youtube.FormatGvsTokenMandatory — not itag 18, not a Premium session's web
// family) is missing_pot without one; a guessed binding counts as no token
// (owner decision D12); and the live strategies degrade, skip or fall to HLS
// on a missing token, as the VOD path does.

// vod18Info is a VOD pool whose only renditions are web_creator's: a
// 1080p + audio split and the muxed 360p itag 18, none with a token-free
// shadow — the members-only or embedding-disabled VOD of audit G31, where
// only WEB-family clients answered.
func vod18Info() *youtube.VideoInfo {
	w, h, w18, h18, fps := 1920, 1080, 640, 360, 30
	return &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		Formats: []youtube.Format{
			{Itag: 137, URL: "http://127.0.0.1:1/videoplayback?itag=137", MimeType: `video/mp4; codecs="avc1.640028"`, Bitrate: 4_000_000, Width: &w, Height: &h, Fps: &fps, ContentLength: "4000", Source: "web_creator"},
			{Itag: 140, URL: "http://127.0.0.1:1/videoplayback?itag=140", MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128_000, AudioQuality: "AUDIO_QUALITY_MEDIUM", ContentLength: "400", Source: "web_creator"},
			{Itag: 18, URL: "http://127.0.0.1:1/videoplayback?itag=18", MimeType: `video/mp4; codecs="avc1.42001E, mp4a.40.2"`, Bitrate: 500_000, Width: &w18, Height: &h18, Fps: &fps, AudioQuality: "AUDIO_QUALITY_LOW", ContentLength: "500", Source: "web_creator"},
		},
	}
}

// TestDownloadVodKeepsAWebFamilyItag18 pins audit G31: itag 18 is
// token-optional, as upstream's require_po_token exempts it
// (`stream_id[0] not in ['18']`, _video.py:3516-3518). A missing_pot degrade
// keeps a web-family 18 and saves the muxed 360p bare, where it used to fail
// "no formats usable without a GVS PO token"; and an 18 picked beside a
// minted token carries it, since its policy still recommends one.
//
// Mutants (each run): dropping the itag 18 exemption from
// withoutGvsRequiredFormats fails the degrade row (the job errors); gating
// the token on the mandatory test instead of GvsTokenRequired fails the
// minted row (the 18 rides bare).
func TestDownloadVodKeepsAWebFamilyItag18(t *testing.T) {
	t.Run("a failed mint saves the 360p bare", func(t *testing.T) {
		calls := fakeVodMint(t, "", errors.New("sidecar down"))
		job, logs := vodPotJob(t)
		res, err := DownloadVod(context.Background(), job, vod18Info(), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
		if err != nil {
			t.Fatalf("DownloadVod = %v, want the web-family 18 downloaded bare", err)
		}
		if res.VideoFormat == nil || res.VideoFormat.Itag != 18 || res.VideoFormat.Source != "web_creator" {
			t.Fatalf("video = %+v, want web_creator's itag 18", res.VideoFormat)
		}
		if res.AudioDownloader != nil {
			t.Errorf("an audio downloader beside a muxed 18: %+v", res.AudioFormat)
		}
		if got := res.VideoDownloader.PoToken(); got != "" {
			t.Errorf("18 downloader token = %q, want none — no token was minted", got)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("mint ran %d times, want 1", n)
		}
		if line := logLine(logs, "[POT] missing_pot: no GVS token"); line == nil || line["dropped"] != 2 {
			t.Errorf("missing_pot line = %v, want one dropping the two formats whose token is mandatory", line)
		}
	})

	t.Run("a minted token rides the 18", func(t *testing.T) {
		fakeVodMint(t, "tok123", nil)
		job, _ := vodPotJob(t)
		info := vod18Info()
		info.Formats = info.Formats[2:]
		res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
		if err != nil {
			t.Fatalf("DownloadVod: %v", err)
		}
		if res.VideoFormat == nil || res.VideoFormat.Itag != 18 {
			t.Fatalf("video = %+v, want itag 18", res.VideoFormat)
		}
		if got := res.VideoDownloader.PoToken(); got != "tok123" {
			t.Errorf("18 downloader token = %q, want tok123 — attached when minted", got)
		}
	})
}

// TestDownloadVodPremiumWaivesTheToken pins owner decision D11 on the VOD
// path: a Premium session's web-family formats are only recommended a token
// (not_required_for_premium, _video.py:3267-3271), so a failed mint keeps the
// selection and downloads it bare — the Premium renditions included — where
// it used to drop the web family for tv's lesser copies. A minted token is
// still attached.
//
// Mutants (each run): ignoring IsPremium in the mandatory test fails the
// failed-mint row (the tv_public pair is chosen); not attaching a minted token
// to a Premium stream fails the minted row.
func TestDownloadVodPremiumWaivesTheToken(t *testing.T) {
	t.Run("a failed mint keeps the web-family pair, bare", func(t *testing.T) {
		fakeVodMint(t, "", errors.New("sidecar down"))
		job, logs := vodPotJob(t)
		info := vodMixedInfo()
		info.IsPremium = true
		res := runVodPot(t, job, info, &bgutils.PotProvider{})
		if res.VideoFormat.Itag != 303 || res.AudioFormat.Itag != 251 {
			t.Errorf("formats = %d / %d, want the web_creator pair 303 + 251", res.VideoFormat.Itag, res.AudioFormat.Itag)
		}
		if res.VideoDownloader.PoToken() != "" || res.AudioDownloader.PoToken() != "" {
			t.Errorf("tokens = %q / %q, want none", res.VideoDownloader.PoToken(), res.AudioDownloader.PoToken())
		}
		if l := logLine(logs, "[POT] missing_pot"); l != nil {
			t.Errorf("a Premium stream went missing_pot: %v", l)
		}
		if l := logLine(logs, "[POT] no GVS token — the chosen web-family formats ride bare"); l == nil || l["premium"] != true {
			t.Errorf("no ride-bare line naming Premium: %v", logs.lines())
		}
	})

	t.Run("a minted token is attached", func(t *testing.T) {
		fakeVodMint(t, "tok123", nil)
		job, _ := vodPotJob(t)
		info := vodMixedInfo()
		info.IsPremium = true
		res := runVodPot(t, job, info, &bgutils.PotProvider{})
		if res.VideoDownloader.PoToken() != "tok123" || res.AudioDownloader.PoToken() != "tok123" {
			t.Errorf("tokens = %q / %q, want tok123 on both", res.VideoDownloader.PoToken(), res.AudioDownloader.PoToken())
		}
	})
}

// TestDownloadVodGuessedBindingCountsAsMissing pins owner decision D12 on the
// VOD path: signed in without a datasync ID, the binding is a guess, and
// yt-dlp mints nothing (fetch_po_token, _video.py:2815-2819). So the
// selection degrades to the token-free copies without a mint at all; only
// when a chosen stream has no token-free copy left — nothing at all, one
// stream of the two, or a pinned rendition — is the token minted under the
// guess, and it rides the first selection (DownloadVod runs the selection
// once more with the guess trusted); a mint that fails there too fails the
// job with the degrade's own error.
//
// Mutants (each run): minting under the guess at once (strategyGvsToken
// ignoring GvsBindingGuessed) fails the degrade row (web_creator chosen, one
// mint); dropping the last resort fails the web-only row (the job errors);
// re-running with the guess still distrusted fails it too.
func TestDownloadVodGuessedBindingCountsAsMissing(t *testing.T) {
	guessed := func(info *youtube.VideoInfo) *youtube.VideoInfo {
		info.GvsBinding, info.GvsBindingKind, info.GvsBindingGuessed = "vd-guess", youtube.BindingVisitorData, true
		return info
	}

	t.Run("token-free copies are preferred, with no mint", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, guessed(vodMixedInfo()), &bgutils.PotProvider{})
		if res.VideoFormat.Source != "tv_public" || res.AudioFormat.Source != "tv_public" {
			t.Errorf("formats from %s / %s, want the tv_public pair", res.VideoFormat.Source, res.AudioFormat.Source)
		}
		if res.VideoDownloader.PoToken() != "" || res.AudioDownloader.PoToken() != "" {
			t.Errorf("tokens = %q / %q, want none", res.VideoDownloader.PoToken(), res.AudioDownloader.PoToken())
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("mint ran %d times, want 0 — nothing is minted under a guess while a token-free copy exists", n)
		}
		if line := logLine(logs, "[POT] missing_pot: no GVS token"); line == nil || !errors.Is(line["err"].(error), errGuessedGvsBinding) {
			t.Errorf("missing_pot line = %v, want one naming the guessed binding", line)
		}
	})

	t.Run("nothing token-free: the guessed token rides the first selection", func(t *testing.T) {
		var bindings []string
		var calls atomic.Int32
		orig := mintGvsPoToken
		mintGvsPoToken = func(_ context.Context, _ *bgutils.PotProvider, binding string) (string, error) {
			calls.Add(1)
			bindings = append(bindings, binding)
			return "tok-guess", nil
		}
		t.Cleanup(func() { mintGvsPoToken = orig })
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, guessed(vodPotInfo("web_creator", "web_creator")), &bgutils.PotProvider{})
		if res.VideoFormat.Itag != 302 || res.AudioFormat.Itag != 251 || res.VideoFormat.Source != "web_creator" {
			t.Errorf("formats = %+v / %+v, want the first selection, web_creator 302 + 251", res.VideoFormat, res.AudioFormat)
		}
		if res.VideoDownloader.PoToken() != "tok-guess" || res.AudioDownloader.PoToken() != "tok-guess" {
			t.Errorf("tokens = %q / %q, want tok-guess on both", res.VideoDownloader.PoToken(), res.AudioDownloader.PoToken())
		}
		if calls.Load() != 1 || bindings[0] != "vd-guess" {
			t.Errorf("mints = %d under %v, want one under the guessed binding", calls.Load(), bindings)
		}
		if line := logLine(logs, "[POT] missing_pot: a chosen stream has no token-free copy"); line == nil || !errors.Is(line["err"].(error), errGuessedGvsBinding) {
			t.Errorf("last-resort line = %v, want one carrying the first run's guessed-binding failure", line)
		}
	})

	// A degrade that keeps one stream and loses the other, or loses a pinned
	// rendition, has left a chosen stream with no token-free copy as surely as
	// one that leaves nothing: the guessed token rides the first selection, so
	// the job does not archive the survivor alone, or another itag in the
	// pin's place. A lesser rendition of the same kind is a token-free copy
	// (the first row above), and so is a pin's own shadow. A token none could
	// be minted for at all is not a guess, and its degrade is left as it was.
	//
	// Mutants (each run): dropping the lost-stream test fails the web-video
	// and web-audio rows (the survivor alone, no mint); dropping the lost-pin
	// test fails the pin row (tv_public 247 recorded, no mint); testing a pin
	// by the selection's itag, not the pool's copy, fails the shadow row (a
	// mint); applying either test to a failed mint fails the last row (the
	// job errors); computing the mandatory test from the video alone fails
	// the web-audio and failed-mint rows (web_creator audio rides bare);
	// reading an audio-only primary as no stream fails the web-video row's
	// cause (video and audio); reading itag 18 as video alone fails the 18
	// row (a mint, the 1080p pair).
	shadowed303 := func() *youtube.VideoInfo {
		info := vodMixedInfo()
		info.Formats[0] = withTokenFreeShadow(info.Formats[0], "tv_public")
		return info
	}
	pin := func(itag int) *int { return &itag }
	for _, tc := range []struct {
		name      string
		info      *youtube.VideoInfo
		pin       *int
		mintErr   error
		wantMints int
		wantVideo string // "itag/source/token"
		wantAudio string // "" for no separate audio downloader
		wantRerun bool
		wantCause string // in the last resort's err
	}{
		{name: "the video has no token-free copy", info: guessed(vodPotInfo("web_creator", "tv_public")), wantMints: 1,
			wantVideo: "302/web_creator/tok-guess", wantAudio: "251/tv_public/", wantRerun: true, wantCause: "the video stream has no token-free copy"},
		{name: "the audio has no token-free copy", info: guessed(vodPotInfo("tv_public", "web_creator")), wantMints: 1,
			wantVideo: "302/tv_public/", wantAudio: "251/web_creator/tok-guess", wantRerun: true, wantCause: "the audio stream has no token-free copy"},
		{name: "a pinned rendition has no token-free copy", info: guessed(vodMixedInfo()), pin: pin(303), wantMints: 1,
			wantVideo: "303/web_creator/tok-guess", wantAudio: "251/web_creator/tok-guess", wantRerun: true, wantCause: "the pinned video itag 303 has no token-free copy"},
		// The muxed itag 18 is token-optional (G31) and carries both streams,
		// so nothing is lost and nothing is minted: the 360p is recorded where
		// the guessed token would have fetched the 1080p pair — upstream's
		// outcome too, since it mints nothing under the guess and keeps 18.
		{name: "the token-free itag 18 carries both streams", info: guessed(vod18Info()), wantVideo: "18/web_creator/"},
		{name: "a pinned rendition's own shadow is its token-free copy", info: guessed(shadowed303()), pin: pin(303),
			wantVideo: "303/tv_public/", wantAudio: "250/tv_public/"},
		{name: "a failed mint is no guess", info: vodPotInfo("tv_public", "web_creator"), mintErr: errors.New("sidecar down"), wantMints: 1,
			wantVideo: "302/tv_public/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "tok-guess"
			if tc.mintErr != nil {
				token = ""
			}
			calls := fakeVodMint(t, token, tc.mintErr)
			job, logs := vodPotJob(t)
			job.Job.SelectedVideoItag = tc.pin
			res, err := DownloadVod(context.Background(), job, tc.info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
			if err != nil {
				t.Fatalf("DownloadVod: %v", err)
			}
			if n := int(calls.Load()); n != tc.wantMints {
				t.Errorf("mint ran %d times, want %d", n, tc.wantMints)
			}
			describe := func(f *youtube.Format, dl interface{ PoToken() string }) string {
				if f == nil || dl == nil {
					return ""
				}
				return fmt.Sprintf("%d/%s/%s", f.Itag, f.Source, dl.PoToken())
			}
			var videoDL, audioDL interface{ PoToken() string }
			if res.VideoDownloader != nil {
				videoDL = res.VideoDownloader
			}
			if res.AudioDownloader != nil {
				audioDL = res.AudioDownloader
			}
			if got := describe(res.VideoFormat, videoDL); got != tc.wantVideo {
				t.Errorf("video = %q, want %q", got, tc.wantVideo)
			}
			if got := describe(res.AudioFormat, audioDL); got != tc.wantAudio {
				t.Errorf("audio = %q, want %q", got, tc.wantAudio)
			}
			last := logLine(logs, "[POT] missing_pot: a chosen stream has no token-free copy")
			if rerun := last != nil; rerun != tc.wantRerun {
				t.Errorf("last resort taken %v, want %v: %v", rerun, tc.wantRerun, logs.lines())
			}
			if last != nil && !strings.Contains(fmt.Sprint(last["err"]), tc.wantCause) {
				t.Errorf("last resort's err = %v, want one saying %q", last["err"], tc.wantCause)
			}
			if logLine(logs, "[POT] missing_pot: no GVS token") == nil {
				t.Errorf("no missing_pot line: %v", logs.lines())
			}
			if tc.wantRerun && logLine(logs, "[FormatSelector] Manual video itag 303 requires a GVS token") != nil {
				t.Errorf("the pin was said to need a token none could be minted, where none was asked for: %v", logs.lines())
			}
		})
	}

	t.Run("a failed mint under the guess fails the job", func(t *testing.T) {
		mintErr := errors.New("sidecar down")
		fakeVodMint(t, "", mintErr)
		job, _ := vodPotJob(t)
		_, err := DownloadVod(context.Background(), job, guessed(vodPotInfo("web_creator", "web_creator")), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
		if !errors.Is(err, mintErr) || !strings.Contains(err.Error(), "no formats usable without a GVS PO token") {
			t.Errorf("err = %v, want the degrade's failure, wrapping the mint's own", err)
		}
	})
}

// withTokenFreeShadow returns f carrying a token-free copy of itself from
// source, at its own URL, as deduplicateFormats leaves a web-family winner
// over a client whose URLs need no GVS token.
func withTokenFreeShadow(f youtube.Format, source string) youtube.Format {
	alt := f
	alt.Source = source
	alt.URL = f.URL + "&c=" + source
	f.TokenFreeAlternate = &alt
	return f
}

// TestDownloadManifestlessDashMissingPot pins audit G16 on the manifest-free
// live path: a chosen stream whose token is mandatory and could not be minted
// is never fetched bare (every segment would 403, and the refresh budget be
// spent re-minting against the same failure). As upstream skips such a
// format before its stream id is recorded (_video.py:3440-3442), the
// selection re-runs over the pool without the formats that need the token,
// their token-free shadows swapped in; the degraded copy is what every later
// lookup by itag reads — the 403 cipher re-resolve too. With nothing left the
// setup fails with errMissingPot (missingPotFallback's input), and a stream
// of the two lost is as much nothing left as both are; a pin the degrade
// dropped is recorded as the token-free pick, and says why; a Premium
// session's streams ride bare; a guessed binding degrades without a mint and
// rides its token only where a chosen stream — or a pinned rendition — has
// no token-free copy (D12), and a mint that fails under the guess too
// degrades on that failure, falling to HLS with nothing left.
//
// Mutants (each run): skipping the degrade fails the shadow and lesser rows
// (web_creator URLs chosen); not rebinding videoInfo to the degraded copy
// fails the shadow row's cipher re-resolve (it hands back the winner's URL);
// not wrapping errMissingPot fails the web-only row; ignoring IsPremium fails
// the Premium row; minting under a guess at once fails the guessed-degrade
// row; dropping the guessed last resort fails the guessed web-only row;
// computing the mandatory test from the video alone, or ignoring a lost
// audio stream, fails the web-audio row (a bare web_creator audio, or none);
// dropping the dropped-pin Warn fails the pin row; dropping the guessed pin
// test fails the guessed-pin row (itag 298 recorded, no mint); degrading
// again under the guess, or not degrading again at all, fails the guessed
// pin's failed-mint row and the guessed failed-mint row.
func TestDownloadManifestlessDashMissingPot(t *testing.T) {
	w720, h720, fps := 1280, 720, 30
	lesser := func(info *youtube.VideoInfo) *youtube.VideoInfo {
		info.Formats = append(info.Formats,
			youtube.Format{Itag: 298, URL: "http://127.0.0.1:1/videoplayback?itag=298&c=tv", MimeType: `video/mp4; codecs="avc1.4d4020"`, Bitrate: 3_000_000, Width: &w720, Height: &h720, Fps: &fps, Source: "tv_auth"},
			youtube.Format{Itag: 139, URL: "http://127.0.0.1:1/videoplayback?itag=139&c=tv", MimeType: `audio/mp4; codecs="mp4a.40.5"`, Bitrate: 48_000, Source: "tv_auth"})
		return info
	}
	shadowed := func(info *youtube.VideoInfo) *youtube.VideoInfo {
		for i := range info.Formats {
			info.Formats[i] = withTokenFreeShadow(info.Formats[i], "tv_auth")
		}
		return info
	}
	premium := func(info *youtube.VideoInfo) *youtube.VideoInfo { info.IsPremium = true; return info }
	guess := func(info *youtube.VideoInfo) *youtube.VideoInfo {
		info.GvsBinding, info.GvsBindingKind, info.GvsBindingGuessed = "vd-guess", youtube.BindingVisitorData, true
		return info
	}
	sidecarDown := errors.New("sidecar down")

	pin299 := 299
	for _, tc := range []struct {
		name                 string
		info                 *youtube.VideoInfo
		pin                  *int
		mintErr              error
		wantErr              bool
		wantMints            int
		wantVideo, wantAudio string // a substring of each downloader's BaseURL
		wantToken            string
		wantLog, forbidLog   string // a log line's prefix
	}{
		{name: "shadows swapped in", info: shadowed(manifestlessPotInfo("web_creator", "web_creator")), mintErr: sidecarDown, wantMints: 1,
			wantVideo: "itag=299&c=tv_auth", wantAudio: "itag=140&c=tv_auth"},
		{name: "a lesser token-free rendition", info: lesser(manifestlessPotInfo("web_creator", "web_creator")), mintErr: sidecarDown, wantMints: 1,
			wantVideo: "itag=298&c=tv", wantAudio: "itag=139&c=tv"},
		{name: "only web-family streams", info: manifestlessPotInfo("web_creator", "web_creator"), mintErr: sidecarDown, wantMints: 1, wantErr: true},
		{name: "Premium rides bare", info: premium(manifestlessPotInfo("web_creator", "web_creator")), mintErr: sidecarDown, wantMints: 1,
			wantVideo: "itag=299", wantAudio: "itag=140"},
		{name: "a guessed binding degrades without a mint", info: guess(lesser(manifestlessPotInfo("web_creator", "web_creator"))), wantMints: 0,
			wantVideo: "itag=298&c=tv", wantAudio: "itag=139&c=tv"},
		{name: "a guessed binding with nothing token-free", info: guess(manifestlessPotInfo("web_creator", "web_creator")), wantMints: 1,
			wantVideo: "itag=299", wantAudio: "itag=140", wantToken: "tok123",
			wantLog: "[POT] missing_pot: a chosen stream has no token-free copy — the token minted under the guessed binding rides"},
		{name: "a web-family audio beside a tv video", info: manifestlessPotInfo("tv_auth", "web_creator"), mintErr: sidecarDown, wantMints: 1, wantErr: true},
		{name: "a pin the degrade dropped", info: lesser(manifestlessPotInfo("web_creator", "web_creator")), pin: &pin299, mintErr: sidecarDown, wantMints: 1,
			wantVideo: "itag=298&c=tv", wantAudio: "itag=139&c=tv",
			wantLog:   "[FormatSelector] Manual video itag 299 requires a GVS token none could be minted; recording itag 298 instead",
			forbidLog: "[FormatSelector] Manual video itag 299 not offered"},
		{name: "a guessed binding with a pin that has no token-free copy", info: guess(lesser(manifestlessPotInfo("web_creator", "web_creator"))), pin: &pin299, wantMints: 1,
			wantVideo: "itag=299", wantAudio: "itag=140", wantToken: "tok123",
			forbidLog: "[FormatSelector] Manual video itag 299 requires a GVS token"},
		{name: "a guessed binding's pin when the mint fails too", info: guess(lesser(manifestlessPotInfo("web_creator", "web_creator"))), pin: &pin299, mintErr: sidecarDown, wantMints: 1,
			wantVideo: "itag=298&c=tv", wantAudio: "itag=139&c=tv",
			wantLog: "[FormatSelector] Manual video itag 299 requires a GVS token none could be minted; recording itag 298 instead"},
		{name: "a guessed binding with nothing token-free when the mint fails too", info: guess(manifestlessPotInfo("web_creator", "web_creator")), mintErr: sidecarDown, wantMints: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "tok123"
			if tc.mintErr != nil {
				token = ""
			}
			calls := fakeVodMint(t, token, tc.mintErr)
			built := recordDownloaders(t)
			job, logs := vodPotJob(t)
			job.Job.SelectedVideoItag = tc.pin
			res, err := DownloadManifestlessDash(context.Background(), job, tc.info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
			if n := int(calls.Load()); n != tc.wantMints {
				t.Errorf("mint ran %d times, want %d", n, tc.wantMints)
			}
			if tc.wantErr {
				if !errors.Is(err, errMissingPot) || !errors.Is(err, tc.mintErr) {
					t.Fatalf("err = %v (result %+v), want errMissingPot wrapping the mint's failure", err, res)
				}
				// The failure a live capture with an HLS manifest falls to it on.
				withHls := *tc.info
				withHls.HlsManifestURL = "https://m.example/x.m3u8"
				if got := missingPotFallback(ManifestlessDashStrategy, &withHls, err); got != HlsStrategy {
					t.Errorf("missingPotFallback = %v, want HlsStrategy", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DownloadManifestlessDash: %v", err)
			}
			if res.VideoDownloader == nil || res.AudioDownloader == nil || len(*built) != 2 {
				t.Fatalf("downloaders built %d, video %v audio %v; want both", len(*built), res.VideoDownloader, res.AudioDownloader)
			}
			for i, c := range []struct{ name, want string }{{"video", tc.wantVideo}, {"audio", tc.wantAudio}} {
				opts := (*built)[i]
				if !strings.HasSuffix(opts.BaseURL, c.want) {
					t.Errorf("%s BaseURL = %q, want one ending %q", c.name, opts.BaseURL, c.want)
				}
				if opts.PoToken != tc.wantToken {
					t.Errorf("%s token = %q, want %q", c.name, opts.PoToken, tc.wantToken)
				}
			}
			// Every row but Premium's is missing_pot: a Premium stream's token
			// is recommended, and its selection is never degraded at all.
			if degraded := logLine(logs, "[POT] missing_pot") != nil; degraded == tc.info.IsPremium {
				t.Errorf("missing_pot logged %v for a session Premium %v: %v", degraded, tc.info.IsPremium, logs.lines())
			}
			if tc.info.IsPremium && logLine(logs, "[POT] no GVS token — the web-family streams ride bare") == nil {
				t.Errorf("no ride-bare line for a Premium session: %v", logs.lines())
			}
			if tc.wantLog != "" && logLine(logs, tc.wantLog) == nil {
				t.Errorf("no %q line: %v", tc.wantLog, logs.lines())
			}
			if tc.forbidLog != "" && logLine(logs, tc.forbidLog) != nil {
				t.Errorf("a %q line: %v", tc.forbidLog, logs.lines())
			}
			if strings.HasSuffix(tc.wantVideo, "c=tv_auth") {
				// The degraded copy is the one the 403 hooks resolve again.
				if got := res.VideoDownloader.OnCipherFailure(); !strings.HasSuffix(got, tc.wantVideo) {
					t.Errorf("cipher re-resolve = %q, want the shadow's URL (…%s)", got, tc.wantVideo)
				}
				if line := logLine(logs, "[POT] missing_pot: serving token-free formats"); line == nil || line["videoSource"] != "tv_auth" || line["audioSource"] != "tv_auth" {
					t.Errorf("serving line = %v, want tv_auth for both", line)
				}
			}
		})
	}
}

// TestDownloadDashMissingPotSkipsTheManifest pins audit G16 on the DASH
// manifest path: a web-family manifest whose token none could be minted for
// is skipped, as upstream skips it (_video.py:3746-3749) — the setup fails
// with errMissingPot before the manifest is fetched — not fetched bare. A
// Premium session's manifest is only recommended a token and is fetched bare;
// a guessed binding (D12) has no token-free copy of a manifest to prefer, so
// the manifest takes the token minted under the guess.
//
// Mutants (each run): fetching bare on a failed mint fails the skip row (one
// request, no error); ignoring IsPremium fails the Premium row (an error);
// withholding the guessed token fails the guessed row (an error and no mint).
func TestDownloadDashMissingPotSkipsTheManifest(t *testing.T) {
	sidecarDown := errors.New("sidecar down")
	for _, tc := range []struct {
		name      string
		mintErr   error
		premium   bool
		guessed   bool
		wantErr   bool
		wantPot   bool
		wantMints int
	}{
		{name: "a failed mint skips the manifest", mintErr: sidecarDown, wantErr: true, wantMints: 1},
		{name: "Premium fetches it bare", mintErr: sidecarDown, premium: true, wantMints: 1},
		{name: "a guessed binding mints under the guess", guessed: true, wantPot: true, wantMints: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "tok123"
			if tc.mintErr != nil {
				token = ""
			}
			calls := fakeVodMint(t, token, tc.mintErr)
			srv := newManifestServer(t, potTestMPD)
			job, _ := vodPotJob(t)
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    srv.URL + "/api/manifest/dash/id/abc",
				DashManifestSource: "web_safari",
				IsPremium:          tc.premium,
				GvsBindingGuessed:  tc.guessed,
			}
			res, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			if n := int(calls.Load()); n != tc.wantMints {
				t.Errorf("mint ran %d times, want %d", n, tc.wantMints)
			}
			srv.mu.Lock()
			paths := append([]string(nil), srv.paths...)
			srv.mu.Unlock()
			if tc.wantErr {
				if !errors.Is(err, errMissingPot) || !errors.Is(err, sidecarDown) {
					t.Errorf("err = %v, want errMissingPot wrapping the mint's failure", err)
				}
				if len(paths) != 0 {
					t.Errorf("manifest fetched %v, want it skipped", paths)
				}
				return
			}
			if err != nil {
				t.Fatalf("DownloadDash: %v", err)
			}
			if len(paths) != 1 || strings.Contains(paths[0], "/pot/") != tc.wantPot {
				t.Errorf("manifest requests = %v, want one, /pot/ %v", paths, tc.wantPot)
			}
			if got := res.VideoDownloader.PoToken() != ""; got != tc.wantPot {
				t.Errorf("video token = %q, want one: %v", res.VideoDownloader.PoToken(), tc.wantPot)
			}
		})
	}
}

// TestDownloadHlsWithholdsAGuessedToken pins D12 on the HLS path: the web
// family's HLS token is recommended, never required (_base.py:85-88), so a
// master whose binding is a guess is fetched bare rather than carry a token
// bound to the guess — upstream mints nothing for a signed-in session without
// a datasync ID — and a mint that fails leaves it bare too.
//
// Mutant: minting under a guess (strategyGvsToken ignoring GvsBindingGuessed)
// fails the guessed row (a mint and a /pot/ master path).
func TestDownloadHlsWithholdsAGuessedToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		guessed   bool
		mintErr   error
		wantMints int
	}{
		{name: "a guessed binding", guessed: true},
		{name: "a failed mint", mintErr: errors.New("sidecar down"), wantMints: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := ""
			if tc.mintErr == nil {
				token = "tok123"
			}
			calls := fakeVodMint(t, token, tc.mintErr)
			srv := newManifestServer(t, potTestMaster)
			job, _ := vodPotJob(t)
			info := &youtube.VideoInfo{
				StreamStatus:      youtube.StreamLive,
				HlsManifestURL:    srv.URL + "/api/manifest/hls_variant/id/abc/file/index.m3u8",
				HlsManifestSource: "web_safari",
				GvsBindingGuessed: tc.guessed,
			}
			res, err := DownloadHls(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			if err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			if n := int(calls.Load()); n != tc.wantMints {
				t.Errorf("mint ran %d times, want %d", n, tc.wantMints)
			}
			if path := srv.onlyPath(t); strings.Contains(path, "/pot/") {
				t.Errorf("master path = %q, want it bare", path)
			}
			if got := res.VideoDownloader.PoToken(); got != "" {
				t.Errorf("downloader token = %q, want none", got)
			}
		})
	}
}

// TestMissingPotFallback pins the routing of a token-less live setup: a live
// capture whose manifest-free or DASH-manifest setup failed with
// errMissingPot falls to HlsStrategy when an HLS manifest came with it (the
// web family's HLS token is recommended only, _base.py:85-88); any other
// failure, a VOD or post-live stream, a stream with no HLS manifest, and an
// HLS or whole-file setup never do.
func TestMissingPotFallback(t *testing.T) {
	missing := errors.Join(errMissingPot, errors.New("sidecar down"))
	live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: "https://m.example/x.m3u8"}
	postLive := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, HlsManifestURL: "https://m.example/x.m3u8"}
	noHls := &youtube.VideoInfo{StreamStatus: youtube.StreamLive}
	for _, tc := range []struct {
		name     string
		strategy DownloadStrategy
		info     *youtube.VideoInfo
		err      error
		want     DownloadStrategy
	}{
		{"manifest-free, live, HLS", ManifestlessDashStrategy, live, missing, HlsStrategy},
		{"DASH manifest, live, HLS", DashStrategy, live, missing, HlsStrategy},
		{"another setup failure", ManifestlessDashStrategy, live, errors.New("no suitable video adaptive format"), nil},
		{"no failure", ManifestlessDashStrategy, live, nil, nil},
		{"post-live", ManifestlessDashStrategy, postLive, missing, nil},
		{"no HLS manifest", ManifestlessDashStrategy, noHls, missing, nil},
		{"HLS itself", HlsStrategy, live, missing, nil},
		{"whole-file VOD", VodStrategy, live, missing, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingPotFallback(tc.strategy, tc.info, tc.err); got != tc.want {
				t.Errorf("missingPotFallback = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeStrategy is a DownloadStrategy that records its call and answers err.
type fakeStrategy struct {
	kind  string
	calls atomic.Int32
	err   error
}

func (f *fakeStrategy) Kind() string { return f.kind }
func (f *fakeStrategy) Download(context.Context, *JobContext, *youtube.VideoInfo, *StrategyDeps) (*DownloadResult, error) {
	f.calls.Add(1)
	return nil, f.err
}

// TestExecuteWithChatFallsToHlsOnMissingPot pins the orchestrator's half of
// the live missing_pot fallback: ExecuteWithChat's first setup, failing with
// errMissingPot on a live stream that carries an HLS manifest, runs the HLS
// strategy next — and the job log says why from-start was given up.
//
// Mutant: dropping the fallback from ExecuteWithChat fails it (HLS never
// runs, and the error is the manifest-free setup's).
func TestExecuteWithChatFallsToHlsOnMissingPot(t *testing.T) {
	hlsErr := errors.New("hls ran")
	mf := &fakeStrategy{kind: "manifestless_dash", err: errors.Join(errMissingPot, errors.New("sidecar down"))}
	hls := &fakeStrategy{kind: "hls", err: hlsErr}
	origMF, origHLS := ManifestlessDashStrategy, HlsStrategy
	ManifestlessDashStrategy, HlsStrategy = mf, hls
	t.Cleanup(func() { ManifestlessDashStrategy, HlsStrategy = origMF, origHLS })

	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "yt_mpfb", VideoID: "mpfb", URL: "https://www.youtube.com/watch?v=mpfb", Platform: "youtube", Status: database.StatusLive}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	logs := &captureLogger{}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: filepath.Join(t.TempDir(), "staging"), Logger: logs}
	info := manifestlessPotInfo("web_creator", "web_creator")
	info.HlsManifestURL, info.HlsManifestSource = "https://m.example/x.m3u8", "web_safari"

	err := w.orchestrator.ExecuteWithChat(context.Background(), jobCtx, info, false, nil)
	if mf.calls.Load() != 1 || hls.calls.Load() != 1 {
		t.Fatalf("manifest-free ran %d, HLS %d times; want one each", mf.calls.Load(), hls.calls.Load())
	}
	if !errors.Is(err, hlsErr) {
		t.Errorf("err = %v, want the HLS setup's", err)
	}
	if logLine(logs, "[POT] missing_pot: no GVS token for the manifestless_dash streams") == nil {
		t.Errorf("no fallback line in the job log: %v", logs.lines())
	}
}

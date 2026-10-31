package worker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
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
// when nothing token-free is left is the token minted under the guess, and it
// rides the first selection (DownloadVod runs the selection once more with
// the guess trusted); a mint that fails there too fails the job with the
// degrade's own error.
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
		if line := logLine(logs, "[POT] missing_pot: nothing token-free to download"); line == nil || !errors.Is(line["err"].(error), errGuessedGvsBinding) {
			t.Errorf("last-resort line = %v, want one carrying the first run's guessed-binding failure", line)
		}
	})

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

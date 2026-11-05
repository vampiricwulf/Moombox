package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The two fallbacks that met when the challenge-free wave (audit G10) was
// merged beside the GVS-policy one (audit G16): a live capture's one
// fallback site, and the whole-file VOD's composed degrades.

// cancelOnSetup is an HLS strategy whose setup succeeds — a result recording
// HLS, nothing written — and then cancels the job, so ExecuteWithChat stops
// at its abort check right after the setup site, and the part can be read as
// the setup site left it.
type cancelOnSetup struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelOnSetup) Kind() string { return "hls" }

func (c *cancelOnSetup) Download(context.Context, *JobContext, *youtube.VideoInfo, *StrategyDeps) (*DownloadResult, error) {
	c.calls++
	c.cancel()
	return &DownloadResult{HasVideo: true, IsHls: true}, nil
}

// TestExecuteWithChatSetsTheDashCaptureAsideForEitherHlsFallback: a live job
// resumed into the part that holds an earlier session's unmuxed from-start
// DASH capture, whose setup now fails — for want of a GVS token (W10's
// missing_pot) or on the player's JS challenges (W8) — records the live HLS
// playlist into that part. ExecuteWithChat has one fallback site for both
// causes, so the DASH capture is set aside before HLS writes either way, to
// be muxed into its own file beside the archive; the missing_pot fallback
// used to be a block of its own after the setup, and its HLS result skipped
// the set-aside, so the finalize muxed the HLS recording and the cleanup
// deleted staging with the DASH one unmuxed in it. Each cause's Warn says
// where HLS records from — the oldest segment the playlist's window holds,
// not the live edge — and a setup error both arms could take falls to HLS
// once, under the missing_pot Warn.
//
// Mutants (each run): the missing_pot fallback kept as its own block in
// ExecuteWithChat after the setup site (the missing_pot rows keep the DASH
// pair in the part); setAsideOtherLiveShape dropped from the setup site (every
// row); the missing_pot Warn back to "from the live edge" (its rows); the
// challenge arm taken ahead of the missing_pot one (the both-causes row logs
// the [Cipher] Warn).
func TestExecuteWithChatSetsTheDashCaptureAsideForEitherHlsFallback(t *testing.T) {
	missingPot := fmt.Errorf("manifestless DASH: %w: no video adaptive format usable without a GVS PO token, and none could be minted: %w", errMissingPot, errors.New("sidecar down"))
	challenges := fmt.Errorf("manifestless DASH: resolve video URL: itag 137: %w: %w", errChallengesUnsolvable, cipher.ErrSidecarUnavailable)
	const potWarn = "[POT] missing_pot: no GVS token for the manifestless_dash streams — recording the live HLS playlist instead"
	const cipherWarn = "[Cipher] live: recording from the start cannot run"
	for _, tc := range []struct {
		name     string
		err      error
		wantWarn string
		notWarn  string
	}{
		{"missing_pot", missingPot, potWarn, cipherWarn},
		{"the challenges cannot be solved", challenges, cipherWarn, potWarn},
		{"both causes at once", errors.Join(missingPot, challenges), potWarn, cipherWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mf := &fakeStrategy{kind: "manifestless_dash", answers: []error{tc.err}}
			hls := &cancelOnSetup{cancel: cancel}
			origMF := ManifestlessDashStrategy
			ManifestlessDashStrategy = mf
			t.Cleanup(func() { ManifestlessDashStrategy = origMF })
			swapHls(t, hls)

			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "yt_both", VideoID: "both", URL: "https://www.youtube.com/watch?v=both", Platform: "youtube", Status: database.StatusLive}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			logs := &captureLogger{}
			rec := &warnRecorder{captureLogger: logs}
			dir := filepath.Join(t.TempDir(), "staging")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// The earlier session's from-start capture, left unmuxed in the
			// part the restart resumes (discoverResumeSegment: the root).
			for _, name := range []string{"video_stream", "audio_stream"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("an earlier session's "+name), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: dir, Logger: rec}
			// Formats that need the player JS (an n), and no service to ask
			// the cookieless chain with: the challenge arm's no-JS from-start
			// path has no pair, so it records HLS
			// (TestLiveChallengeFailureRecordsFromStartWithoutJS has the pair).
			v, a := adaptiveVideo(), adaptiveAudio()
			v.URL += "&n=" + nChal
			a.URL += "&n=" + nChal
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
				HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "tv_public",
				Formats: []youtube.Format{v, a}}

			if err := w.orchestrator.ExecuteWithChat(ctx, jobCtx, info, false, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("ExecuteWithChat = %v, want the cancel the HLS setup made", err)
			}
			if len(mf.seen) != 1 || hls.calls != 1 {
				t.Fatalf("manifest-free ran %d, HLS %d times; want 1 and 1", len(mf.seen), hls.calls)
			}
			for _, name := range []string{"video_stream", "audio_stream"} {
				if fileExists(filepath.Join(dir, name)) {
					t.Errorf("%s is still in the part the HLS capture records into", name)
				}
			}
			if asides := stagedRestartAsides(dir); len(asides) != 2 || len(groupStagedAsides(asides)) != 1 {
				t.Errorf("asides = %v, want the DASH pair set aside as one recording", asides)
			}
			if !rec.warned(tc.wantWarn) || rec.warned(tc.notWarn) {
				t.Errorf("warned %q: %v, %q: %v; want only the first: %v", tc.wantWarn, rec.warned(tc.wantWarn), tc.notWarn, rec.warned(tc.notWarn), logs.lines())
			}
			line := logLine(logs, tc.wantWarn)
			if msg, _ := line[""].(string); !strings.HasSuffix(msg, "from the oldest segment its window holds") {
				t.Errorf("fallback line %q does not say HLS records from the oldest segment its window holds", msg)
			}
			if cause, _ := line["err"].(error); !errors.Is(cause, tc.err) {
				t.Errorf("fallback line's err = %v, want the setup failure", line["err"])
			}
		})
	}
}

// orderBInfo is a VOD pool in which the missing_pot degrade comes first and
// the challenge-free one after it: the best video, web_creator's 303, needs
// the player JS (an n, which the goja fallback can answer) and a GVS token,
// with no token-free copy; the best audio, web_creator's 251, needs no JS but
// a token; and tv_public's lesser 247 and 250 need no token but a
// signature, which only the sidecar can solve. tv outranks the web family,
// so each is a stream of its own, as the dedup leaves them.
func orderBInfo() *youtube.VideoInfo {
	w1080, h1080, w720, h720, fps := 1920, 1080, 1280, 720, 30
	return &youtube.VideoInfo{
		PlayerURL: nPlayer,
		Formats: []youtube.Format{
			withLevel(youtube.Format{Itag: 303, URL: "http://127.0.0.1:1/videoplayback?itag=303&n=" + nChal, MimeType: `video/webm; codecs="vp9"`, Bitrate: 4_000_000, Width: &w1080, Height: &h1080, Fps: &fps, ContentLength: "4000", Source: "web_creator"}, youtube.AuthLevelWebCreator),
			withLevel(youtube.Format{Itag: 247, URL: "http://127.0.0.1:1/videoplayback?itag=247", EncryptedSig: "ENCRYPTEDSIG", MimeType: `video/webm; codecs="vp9"`, Bitrate: 1_500_000, Width: &w720, Height: &h720, Fps: &fps, ContentLength: "1500", Source: "tv_public"}, youtube.AuthLevelTVPublic),
			withLevel(youtube.Format{Itag: 251, URL: "http://127.0.0.1:1/videoplayback?itag=251", MimeType: `audio/webm; codecs="opus"`, Bitrate: 160_000, ContentLength: "160", Source: "web_creator"}, youtube.AuthLevelWebCreator),
			withLevel(youtube.Format{Itag: 250, URL: "http://127.0.0.1:1/videoplayback?itag=250", EncryptedSig: "ENCRYPTEDSIG", MimeType: `audio/webm; codecs="opus"`, Bitrate: 70_000, ContentLength: "70", Source: "tv_public"}, youtube.AuthLevelTVPublic),
		},
	}
}

// TestDownloadVodComposesTheMissingPotAndChallengeFreeDegrades pins the two
// whole-file degrades together, in the order the challenge-free wave did not
// handle: the first selection resolves (the web family's n is answered, its
// audio needs nothing) and the mint fails, so the missing_pot degrade drops
// the web family, re-extracts the cookieless chain for the lost 303 and
// selects tv_public's 247 — whose signature, with the sidecar down, fails
// systemically. That ended the job; DownloadVod's challenge-free degrade now
// answers it as it answers the first selection's failure, and its run
// degrades for the token again over the formats that need no player JS: the
// web_creator 251 that needs none goes for its visionos shadow. Both streams
// are a copy that needs neither the player JS nor a token, from ONE
// cookieless re-extract, on ONE mint.
//
// Mutants (each run): the missing_pot degrade's systemic failure not
// reaching the challenge-free degrade (the job fails); the challenge-free
// degrade given a re-extract of its own (two re-extracts); the mint made
// again by the challenge-free run (two mints); the challenge-free run
// selecting from the extraction's formats (the solver is asked again, and
// the job fails).
func TestDownloadVodComposesTheMissingPotAndChallengeFreeDegrades(t *testing.T) {
	mints := fakeVodMint(t, "", errors.New("sidecar down"))
	info := orderBInfo()
	// The cookieless chain serves the 720p and the audio, with no n and no
	// signature; not the 1080p.
	visionos := func(f youtube.Format) youtube.Format {
		c := cookielessCopy(f)
		c.EncryptedSig = ""
		return c
	}
	rx := fakeCookieless(t, []youtube.Format{visionos(info.Formats[1]), visionos(info.Formats[2])}, nil)
	built := recordDownloaders(t)
	job, logs := reextractJob(t)
	solver := &challengeSolver{sigErr: fmt.Errorf("%w: the sidecar is not running", cipher.ErrSidecarUnavailable)}

	res, err := DownloadVod(context.Background(), job, info, solver, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadVod = %v, want the visionos 247 and 251", err)
	}
	if n := rx.Load(); n != 1 {
		t.Errorf("cookieless re-extract ran %d times, want 1 across both degrades", n)
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("mint ran %d times, want 1: the challenge-free run degrades on the first one's failure", n)
	}
	// The 303's n, then the 247's signature: nothing after the degrade.
	if n := solver.asked.Load(); n != 2 {
		t.Errorf("solver asked %d times, want 2", n)
	}
	for _, f := range []*youtube.Format{res.VideoFormat, res.AudioFormat} {
		if f == nil || f.Source != "visionos" || f.NeedsPlayerJS() || youtube.GvsTokenRequired(f.Source) {
			t.Errorf("recorded format %+v, want a visionos copy that needs neither the player JS nor a token", f)
		}
	}
	if res.VideoFormat == nil || res.VideoFormat.Itag != 247 || res.AudioFormat == nil || res.AudioFormat.Itag != 251 {
		t.Errorf("formats = %+v / %+v, want 247 and 251", res.VideoFormat, res.AudioFormat)
	}
	if len(*built) != 2 || (*built)[0].PoToken != "" || (*built)[1].PoToken != "" ||
		!strings.Contains((*built)[0].BaseURL, "/visionos/") || !strings.Contains((*built)[1].BaseURL, "/visionos/") {
		t.Errorf("downloaders built on %+v, want the visionos URLs, bare", *built)
	}
	if logLine(logs, "[Cipher] VOD: the player's JS challenges cannot be solved") == nil {
		t.Errorf("no challenge-free degrade line: %v", logs.lines())
	}
	if l := logLine(logs, "[POT] missing_pot: serving token-free formats"); l == nil || l["videoSource"] != "visionos" {
		t.Errorf("serving line = %v, want the missing_pot degrade's last selection, visionos", l)
	}
	if info.Formats[0].Source != "web_creator" || len(info.Formats) != 4 {
		t.Error("the caller's VideoInfo was rewritten")
	}
}

// TestDownloadVodGuessedRerunKeepsTheChallengeFreeDegrade: the guessed
// binding's trusted run (D12's last resort) selects from the challenge-free
// formats when that degrade ran, rather than starting over from the
// extraction. tv_public's copies need the player JS (an n no solver answers)
// and each carries a web_creator copy that needs none but a token, which
// under a guessed binding counts as missing; the cookieless chain serves only
// android_vr copies, whose n has no answer either. So the challenge-free run
// is left only the web_creator copies, the missing_pot degrade drops them,
// and nothing token-free and challenge-free is left: the trusted run mints
// under the guess, and the token rides the web_creator copies. The solver is
// asked once, the chain once, the minter once.
//
// Mutants (each run): the trusted run handed a fresh degrade state (the
// extraction's tv copies are resolved again and the chain asked twice); the
// missing_pot merge taking the chain's copies that need the JS (android_vr
// is selected, and the solver asked again).
func TestDownloadVodGuessedRerunKeepsTheChallengeFreeDegrade(t *testing.T) {
	mints := fakeVodMint(t, "tok-guess", nil)
	info := tvChallengeInfo(false)
	info.GvsBinding, info.GvsBindingKind, info.GvsBindingGuessed = "vd-guess", youtube.BindingVisitorData, true
	var vr []youtube.Format
	for i := range info.Formats {
		f := &info.Formats[i]
		wc := withLevel(*f, youtube.AuthLevelWebCreator)
		wc.Source, wc.URL = "web_creator", strings.Replace(strings.TrimSuffix(f.URL, "&n="+nChal), "videoplayback", "wc/videoplayback", 1)
		f.TokenAlternate = &wc
		a := withLevel(*f, youtube.AuthLevelAndroidVR)
		a.Source, a.URL, a.TokenAlternate = "android_vr", strings.Replace(f.URL, "videoplayback", "vr/videoplayback", 1), nil
		vr = append(vr, a)
	}
	rx := fakeCookieless(t, vr, nil)
	built := recordDownloaders(t)
	job, logs := reextractJob(t)
	solver := &challengeSolver{n: sentinelOf}

	res, err := DownloadVod(context.Background(), job, info, solver, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadVod = %v, want the web_creator copies under the guessed token", err)
	}
	if n := solver.asked.Load(); n != 1 {
		t.Errorf("solver asked %d times, want 1: the trusted run starts from the challenge-free formats", n)
	}
	if n := rx.Load(); n != 1 {
		t.Errorf("cookieless re-extract ran %d times, want 1", n)
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("mint ran %d times, want 1 — under the trusted guess only", n)
	}
	for _, f := range []*youtube.Format{res.VideoFormat, res.AudioFormat} {
		if f == nil || f.Source != "web_creator" || f.NeedsPlayerJS() {
			t.Errorf("recorded format %+v, want web_creator's copy that needs no player JS", f)
		}
	}
	if len(*built) != 2 || (*built)[0].PoToken != "tok-guess" || (*built)[1].PoToken != "tok-guess" {
		t.Errorf("downloaders built on %+v, want the guessed token on both", *built)
	}
	last := logLine(logs, "[POT] missing_pot: a chosen stream has no token-free copy")
	if last == nil {
		t.Fatalf("no last-resort line: %v", logs.lines())
	}
	if cause, _ := last["err"].(error); !errors.Is(cause, errGuessedGvsBinding) || !errors.Is(cause, errChallengesUnsolvable) {
		t.Errorf("last resort's err = %v, want one naming the guessed binding and the challenges", last["err"])
	}
}

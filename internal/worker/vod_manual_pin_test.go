package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// warnRecorder is captureLogger with each Warn's message kept apart as well,
// so a test can hold a line to its level — captureLogger records every level
// alike, and the pin's verdict is a Warn on the whole-file path.
type warnRecorder struct {
	*captureLogger
	mu    sync.Mutex
	warns []string
}

func (w *warnRecorder) Warn(msg string, args ...any) {
	w.mu.Lock()
	w.warns = append(w.warns, msg)
	w.mu.Unlock()
	w.captureLogger.Warn(msg, args...)
}

// warnCount is how many lines exactly msg were logged at Warn.
func (w *warnRecorder) warnCount(msg string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.warns {
		if m == msg {
			n++
		}
	}
	return n
}

// warned reports whether a line starting with prefix was logged at Warn.
func (w *warnRecorder) warned(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.warns, func(m string) bool { return strings.HasPrefix(m, prefix) })
}

// vodPinJob is vodPotJob with its logger behind a warnRecorder.
func vodPinJob(t *testing.T) (*JobContext, *warnRecorder) {
	t.Helper()
	job, logs := vodPotJob(t)
	rec := &warnRecorder{captureLogger: logs}
	job.Logger = rec
	return job, rec
}

// TestDownloadVodSaysWhyASegmentedPinIsNotTaken is the owner's W4 picker
// follow-up on the whole-file path. The format picker offers a segmented
// rendition — marked so — because the manifest-free live and post-live paths
// fetch it as fragments; a whole-file VOD leaves it out of its pool
// (wholeFileVodPool), so a pin of one there fell back to automatic selection
// with "Manual video itag N not found", though the picker had just listed
// it. The warning now says why: it is segmented, not available as a whole
// file — on the first selection, and on a missing_pot degrade's re-run too,
// which the automatic pick's token-requiring file sets off.
//
// Mutants (each run): the segmented arm dropped from the video pin or from
// the audio pin (that row's line is "not found" again); the first selection,
// or the degraded one, not handed the segmented formats (every row, or the
// degrade row's second line, says "not found"); the video arm's line logged
// at Debug (the video rows find no Warn).
func TestDownloadVodSaysWhyASegmentedPinIsNotTaken(t *testing.T) {
	for _, tc := range []struct {
		name         string
		video, audio int
		formats      []youtube.Format
		mintFails    bool // a GVS mint that fails, so the selection re-runs degraded
		wantLine     string
		wantVideo    int
		wantAudio    int
	}{
		{name: "a video pin", video: 137,
			formats:  []youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)},
			wantLine: "[FormatSelector] Manual video itag 137 is segmented, not available as a whole file; falling back to auto", wantVideo: 136, wantAudio: 140},
		{name: "an audio pin", audio: 251,
			formats:  []youtube.Format{vodFormat(136, 1280, 720, h264, withLength), vodFormat(251, 0, 0, `audio/webm; codecs="opus"`, asLiveAdaptive), vodFormat(140, 0, 0, aac, withLength)},
			wantLine: "[FormatSelector] Manual audio itag 251 is segmented, not available as a whole file; falling back to auto", wantVideo: 136, wantAudio: 140},
		{name: "a video pin through a missing_pot degrade", video: 137, mintFails: true,
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, asOTF),
				vodFormat(136, 1280, 720, h264, withLength, func(f *youtube.Format) { f.Source = "watch_page" }),
				vodFormat(135, 854, 480, h264, withLength),
				vodFormat(140, 0, 0, aac, withLength),
			},
			wantLine: "[FormatSelector] Manual video itag 137 is segmented, not available as a whole file; falling back to auto", wantVideo: 135, wantAudio: 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, logs := vodPinJob(t)
			if tc.video != 0 {
				job.Job.SelectedVideoItag = &tc.video
			}
			if tc.audio != 0 {
				job.Job.SelectedAudioItag = &tc.audio
			}
			var pot *bgutils.PotProvider
			if tc.mintFails {
				fakeVodMint(t, "", errors.New("sidecar down"))
				pot = &bgutils.PotProvider{}
			}
			res, err := DownloadVod(context.Background(), job, segmentedVodInfo(tc.formats...), stubCipherSolver{}, nil, pot, nil)
			if err != nil {
				t.Fatalf("DownloadVod: %v", err)
			}
			if tc.mintFails && logLine(logs.captureLogger, "[POT] missing_pot: no GVS token") == nil {
				t.Fatalf("the degrade never ran: %v", logs.lines())
			}
			if res.VideoFormat == nil || res.VideoFormat.Itag != tc.wantVideo || res.AudioFormat == nil || res.AudioFormat.Itag != tc.wantAudio {
				t.Fatalf("video %+v audio %+v, want the automatic %d + %d", res.VideoFormat, res.AudioFormat, tc.wantVideo, tc.wantAudio)
			}
			if !logs.warned(tc.wantLine) {
				t.Errorf("no %q at Warn: %v", tc.wantLine, logs.lines())
			}
			if l := logLines(logs.captureLogger, func(m string) bool { return strings.Contains(m, "not found, falling back") }); len(l) > 0 {
				t.Errorf("still says not found: %v", l)
			}
		})
	}
}

// TestDownloadVodNamesTheTokenBeforeTheSegments: a missing_pot degrade swaps
// a pinned stream's web-family file for its token-free shadow, and where that
// shadow is segmented the re-filter leaves it out too — so the pinned itag
// is in both lists the warning reads. Its whole file is the copy the token
// was needed for, so the token is what the warning names.
//
// Mutant: the segmented arm ahead of the token one (the line says
// segmented).
func TestDownloadVodNamesTheTokenBeforeTheSegments(t *testing.T) {
	fakeVodMint(t, "", errors.New("sidecar down"))
	shadow := vodFormat(137, 1920, 1080, h264, asLiveAdaptive)
	winner := vodFormat(137, 1920, 1080, h264, withLength, func(f *youtube.Format) { f.Source = "watch_page"; f.TokenFreeAlternate = &shadow })
	job, logs := vodPinJob(t)
	pin := 137
	job.Job.SelectedVideoItag = &pin
	res, err := DownloadVod(context.Background(), job, segmentedVodInfo(winner, vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)),
		stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadVod: %v", err)
	}
	if res.VideoFormat == nil || res.VideoFormat.Itag != 136 {
		t.Fatalf("video %+v, want the 720p file", res.VideoFormat)
	}
	if !logs.warned("[FormatSelector] Manual video itag 137 requires a GVS token none could be minted") {
		t.Errorf("no token line at Warn: %v", logs.lines())
	}
	if l := logLine(logs.captureLogger, "[FormatSelector] Manual video itag 137 is segmented"); l != nil {
		t.Errorf("named the shadow's segments over the token: %v", l)
	}
}

// TestDownloadVodHonoursADamagedPin is the owner's W9 picker follow-up and
// its second PK follow-up: the picker marks a damaged rendition and badges an
// intact one, but a damaged one can still be pinned by hand. Damage only
// ranks a rendition lower — it stays in the pool — so the pin is found and
// downloaded; it did so without a word, where the archive's track then
// stopped partway. The job log now says so, at Warn, once: "Manual video
// itag N is possibly damaged (shorter than the video); recording it as
// pinned", or audio — once however many times the selection ran to settle
// the formats (the cipher re-selection an unresolvable sibling sets off, a
// missing_pot degrade), and only of a pin the job records: an audio pin with
// no video is recorded in the video's place, and says so; a damaged pin
// whose own signature failed, which the re-selection replaced, an audio pin
// beside a muxed video, whose audio rides inside it, a video pin of an itag
// only an audio format has, and an intact pin say nothing. The verdict is
// the recorded copy's own: a degrade that swaps a pinned web_creator file
// for its token-free shadow (withoutGvsRequiredFormats) records the shadow,
// which the extraction stamped from its own response, so a damaged file
// with an intact shadow says nothing and an intact file with a damaged
// shadow says so.
//
// Mutants (each run): warnDamagedPins not called (no row finds its line);
// the line logged at Info (no row finds it at Warn); the line logged from
// selectVodFormats on every selection that honours the pin instead of from
// the settled result (the re-selection and degrade rows find it twice, the
// own-signature row finds it of a pick no download used, the muxed row of an
// audio pin the job does not record); the line logged at Debug (as at
// Info); the Damaged test dropped (the intact and muxed rows warn of intact
// formats); the kind test dropped (the
// audio-itag row warns of a video pin); the audio pin unread (the audio
// rows find no line); the recorded video format unread (the video rows and
// the no-video row find none); warnDamagedPins called ahead of the GVS
// block, so it reads the pre-degrade pick (the two shadow rows: the damaged
// file's shadow warns, the damaged shadow's file does not).
func TestDownloadVodHonoursADamagedPin(t *testing.T) {
	damaged := func(f *youtube.Format) { f.Damaged = true }
	sig := func(f *youtube.Format) { f.EncryptedSig = "SIG" }
	// shadowed makes f a web_creator file — one a GVS token is required for
	// — whose token-free tv_public shadow is damaged or not, the shape the
	// dedup leaves when two clients list the same itag.
	shadowed := func(damagedShadow bool) func(*youtube.Format) {
		return func(f *youtube.Format) {
			shadow := vodFormat(f.Itag, 1920, 1080, h264, withLength)
			shadow.Damaged = damagedShadow
			f.Source, f.TokenFreeAlternate = "web_creator", &shadow
		}
	}
	const (
		videoLine = "[FormatSelector] Manual video itag 137 is possibly damaged (shorter than the video); recording it as pinned"
		audioLine = "[FormatSelector] Manual audio itag 140 is possibly damaged (shorter than the video); recording it as pinned"
	)
	for _, tc := range []struct {
		name         string
		video, audio int // the pins, 0 for none
		formats      []youtube.Format
		solver       cipher.Solver
		mintFails    bool // a GVS mint that fails, so the selection re-runs degraded
		wantVideo    int
		wantAudio    int // 0: no separate audio
		wantLine     string
		wantNoLine   bool
		wantReselect string // a re-selection the row sets off, by its success line
		wantSource   string // the recorded video copy's client, when the row holds one
	}{
		{name: "a video pin", video: 137,
			formats:   []youtube.Format{vodFormat(137, 1920, 1080, h264, withLength, damaged), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)},
			wantVideo: 137, wantAudio: 140, wantLine: videoLine},
		{name: "an audio pin", audio: 140,
			formats:   []youtube.Format{vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength, damaged), vodFormat(139, 0, 0, aac, withLength)},
			wantVideo: 136, wantAudio: 140, wantLine: audioLine},
		{name: "a video pin through the audio's cipher re-selection", video: 137, solver: failingSigSolver{},
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, withLength, damaged),
				vodFormat(136, 1280, 720, h264, withLength),
				vodFormat(140, 0, 0, aac, withLength, sig, func(f *youtube.Format) { f.Bitrate = 900_000 }),
				vodFormat(139, 0, 0, aac, withLength),
			},
			wantVideo: 137, wantAudio: 139, wantLine: videoLine, wantReselect: "[Cipher] VOD audio re-selection succeeded"},
		{name: "an audio pin through the video's cipher re-selection", audio: 140, solver: failingSigSolver{},
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, withLength, sig),
				vodFormat(136, 1280, 720, h264, withLength),
				vodFormat(140, 0, 0, aac, withLength, damaged),
				vodFormat(139, 0, 0, aac, withLength),
			},
			wantVideo: 136, wantAudio: 140, wantLine: audioLine, wantReselect: "[Cipher] VOD video re-selection succeeded"},
		{name: "a video pin through a missing_pot degrade", video: 137, mintFails: true,
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, withLength, damaged),
				vodFormat(140, 0, 0, aac, withLength, func(f *youtube.Format) { f.Source = "watch_page"; f.Bitrate = 900_000 }),
				vodFormat(139, 0, 0, aac, withLength),
			},
			wantVideo: 137, wantAudio: 139, wantLine: videoLine},
		{name: "a video pin whose degrade swaps a damaged file for an intact shadow", video: 137, mintFails: true,
			formats:   []youtube.Format{vodFormat(137, 1920, 1080, h264, withLength, damaged, shadowed(false)), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)},
			wantVideo: 137, wantAudio: 140, wantSource: "tv_public", wantNoLine: true},
		{name: "a video pin whose degrade swaps an intact file for a damaged shadow", video: 137, mintFails: true,
			formats:   []youtube.Format{vodFormat(137, 1920, 1080, h264, withLength, shadowed(true)), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)},
			wantVideo: 137, wantAudio: 140, wantSource: "tv_public", wantLine: videoLine},
		{name: "a damaged pin whose own signature fails", video: 137, solver: failingSigSolver{},
			formats:   []youtube.Format{vodFormat(137, 1920, 1080, h264, withLength, damaged, sig), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)},
			wantVideo: 136, wantAudio: 140, wantNoLine: true, wantReselect: "[Cipher] VOD video re-selection succeeded"},
		{name: "an audio pin beside a muxed video", video: 18, audio: 140,
			formats:   []youtube.Format{vodFormat(18, 640, 360, muxed18, withLength, func(f *youtube.Format) { f.AudioQuality = "AUDIO_QUALITY_LOW" }), vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength, damaged)},
			wantVideo: 18, wantNoLine: true},
		{name: "an audio pin with no video", video: -1, audio: 140,
			formats:   []youtube.Format{vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength, damaged), vodFormat(139, 0, 0, aac, withLength)},
			wantVideo: 140, wantLine: audioLine},
		{name: "a video pin of an itag only an audio format has", video: 140,
			formats:   []youtube.Format{vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength, damaged)},
			wantVideo: 136, wantAudio: 140, wantNoLine: true},
		{name: "an intact pin", video: 137, audio: 140,
			formats:   []youtube.Format{vodFormat(137, 1920, 1080, h264, withLength), vodFormat(136, 1280, 720, h264, withLength, damaged), vodFormat(140, 0, 0, aac, withLength), vodFormat(139, 0, 0, aac, withLength, damaged)},
			wantVideo: 137, wantAudio: 140, wantNoLine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, logs := vodPinJob(t)
			if tc.video != 0 {
				job.Job.SelectedVideoItag = &tc.video
			}
			if tc.audio != 0 {
				job.Job.SelectedAudioItag = &tc.audio
			}
			solver := tc.solver
			if solver == nil {
				solver = stubCipherSolver{}
			}
			var pot *bgutils.PotProvider
			if tc.mintFails {
				fakeVodMint(t, "", errors.New("sidecar down"))
				pot = &bgutils.PotProvider{}
			}
			res, err := DownloadVod(context.Background(), job, segmentedVodInfo(tc.formats...), solver, nil, pot, nil)
			if err != nil {
				t.Fatalf("DownloadVod: %v", err)
			}
			if tc.mintFails && logLine(logs.captureLogger, "[POT] missing_pot: no GVS token") == nil {
				t.Fatalf("the degrade never ran: %v", logs.lines())
			}
			if tc.wantReselect != "" && logLine(logs.captureLogger, tc.wantReselect) == nil {
				t.Fatalf("the re-selection never ran: %v", logs.lines())
			}
			gotAudio := 0
			if res.AudioFormat != nil {
				gotAudio = res.AudioFormat.Itag
			}
			if res.VideoFormat == nil || res.VideoFormat.Itag != tc.wantVideo || gotAudio != tc.wantAudio {
				t.Fatalf("video %+v audio %+v, want %d + %d", res.VideoFormat, res.AudioFormat, tc.wantVideo, tc.wantAudio)
			}
			if tc.wantSource != "" && res.VideoFormat.Source != tc.wantSource {
				t.Fatalf("recorded the %s copy of video %d, want the %s shadow", res.VideoFormat.Source, res.VideoFormat.Itag, tc.wantSource)
			}
			damagedLines := logLines(logs.captureLogger, func(m string) bool { return strings.Contains(m, "is possibly damaged (shorter than the video)") })
			if tc.wantNoLine {
				if len(damagedLines) > 0 {
					t.Errorf("warned of a pin the job does not record damaged: %v", damagedLines)
				}
				return
			}
			if n := logs.warnCount(tc.wantLine); n != 1 || len(damagedLines) != 1 {
				t.Errorf("%q logged %d times at Warn, %d at any level; want once, at Warn: %v", tc.wantLine, n, len(damagedLines), logs.lines())
			}
		})
	}
}

// TestDownloadVodReselectionKeepsThePinVerdict: when the automatic pick a
// pin fell back to fails its signature, the cipher re-selection
// (reselectVodWithout) re-runs the job's selection — without the segmented
// or token-dropped formats the first run was handed, so the pin loop said
// "Manual video itag N not found, falling back to auto" of the itag the line
// before had called segmented, or token-bound, and the job log contradicted
// itself. The re-selection is handed the same lists now, and says the same.
//
// Mutants (each run): reselectVodWithout handing selectVodFormats nil for
// segmented (the first row says "not found"), or for dropped (the second);
// resolveVodURLs handing the degraded selection's dropped list on as nil
// (the second).
func TestDownloadVodReselectionKeepsThePinVerdict(t *testing.T) {
	sig := func(f *youtube.Format) { f.EncryptedSig = "SIG" }
	for _, tc := range []struct {
		name      string
		formats   []youtube.Format
		mintFails bool
		wantLine  string
	}{
		{name: "a segmented pin",
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, asOTF),
				vodFormat(136, 1280, 720, h264, withLength, sig),
				vodFormat(135, 854, 480, h264, withLength),
				vodFormat(140, 0, 0, aac, withLength),
			},
			wantLine: "[FormatSelector] Manual video itag 137 is segmented, not available as a whole file; falling back to auto"},
		{name: "a token-dropped pin", mintFails: true,
			formats: []youtube.Format{
				vodFormat(137, 1920, 1080, h264, withLength, func(f *youtube.Format) { f.Source = "watch_page" }),
				vodFormat(136, 1280, 720, h264, withLength, sig),
				vodFormat(135, 854, 480, h264, withLength),
				vodFormat(140, 0, 0, aac, withLength),
			},
			wantLine: "[FormatSelector] Manual video itag 137 requires a GVS token none could be minted; falling back to auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, logs := vodPinJob(t)
			pin := 137
			job.Job.SelectedVideoItag = &pin
			var pot *bgutils.PotProvider
			if tc.mintFails {
				fakeVodMint(t, "", errors.New("sidecar down"))
				pot = &bgutils.PotProvider{}
			}
			res, err := DownloadVod(context.Background(), job, segmentedVodInfo(tc.formats...), failingSigSolver{}, nil, pot, nil)
			if err != nil {
				t.Fatalf("DownloadVod: %v", err)
			}
			if tc.mintFails && logLine(logs.captureLogger, "[POT] missing_pot: no GVS token") == nil {
				t.Fatalf("the degrade never ran: %v", logs.lines())
			}
			if logLine(logs.captureLogger, "[Cipher] VOD video re-selection succeeded") == nil {
				t.Fatalf("the re-selection never ran: %v", logs.lines())
			}
			if res.VideoFormat == nil || res.VideoFormat.Itag != 135 {
				t.Fatalf("video %+v, want 135, the alternate to the unresolvable 136", res.VideoFormat)
			}
			if !logs.warned(tc.wantLine) {
				t.Errorf("no %q at Warn: %v", tc.wantLine, logs.lines())
			}
			if l := logLines(logs.captureLogger, func(m string) bool { return strings.Contains(m, "not found, falling back") }); len(l) > 0 {
				t.Errorf("the re-selection says not found: %v", l)
			}
		})
	}
}

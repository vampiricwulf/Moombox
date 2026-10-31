package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
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

// TestDownloadVodHonoursADamagedPin is the owner's W9 picker follow-up: the
// picker marks a damaged rendition and badges an intact one, but a damaged
// one can still be pinned by hand. Damage only ranks a rendition lower — it
// stays in the pool — so the pin is found and downloaded, with no warning.
//
// Mutant: the manual override passing Damaged formats over (the pin falls
// back to the intact 136 and warns "not found").
func TestDownloadVodHonoursADamagedPin(t *testing.T) {
	job, logs := vodPotJob(t)
	pin := 137
	job.Job.SelectedVideoItag = &pin
	damaged := vodFormat(137, 1920, 1080, h264, withLength, func(f *youtube.Format) { f.Damaged = true })
	res, err := DownloadVod(context.Background(), job, segmentedVodInfo(damaged, vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength)),
		stubCipherSolver{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("DownloadVod: %v", err)
	}
	if res.VideoFormat == nil || res.VideoFormat.Itag != 137 {
		t.Errorf("video %+v, want the pinned damaged 137", res.VideoFormat)
	}
	if l := logLines(logs, func(m string) bool { return strings.HasPrefix(m, "[FormatSelector] Manual video itag 137") }); len(l) > 0 {
		t.Errorf("a honoured pin warned: %v", l)
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

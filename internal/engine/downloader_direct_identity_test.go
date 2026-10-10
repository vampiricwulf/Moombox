package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestResumeIdentityWholeFileVodURL pins the whole-file VOD shape of the URL
// fingerprint. A finished VOD's format URL carries `id=o-…`, an opaque token
// with no `.N` stream suffix, so streamIdentity used to extract nothing from
// it: every whole-file resume compared "" with "" and was trusted, and a
// restart whose selection now picked another itag appended that rendition's
// bytes to the old one's checkpoint. The fingerprint is the itag plus `clen`;
// the `o-` token and every session parameter may rotate.
//
// Mutant: `streamIdentityWholeFileIDRe.MatchString(rawURL)` → `false` in
// streamIdentity — the different-itag case reads as the same stream. Mutant:
// dropping the `clen` append — the re-encode case does.
func TestResumeIdentityWholeFileVodURL(t *testing.T) {
	const (
		vod399     = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=399&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod399Next = "https://rr4---sn-b.googlevideo.com/videoplayback?expire=1700021600&ei=BBB&ip=5.6.7.8&id=o-BQrotated99&itag=399&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod137     = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=137&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod399Redo = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=399&source=youtube&mime=video%2Fmp4&clen=81920000&dur=600.000"
	)
	for _, tc := range []struct {
		name         string
		saved, now   string
		wantMismatch bool
	}{
		{"same rendition, every session parameter and the o- token rotated — resume", vod399, vod399Next, false},
		{"another itag — a different rendition, start over", vod399, vod137, true},
		{"same itag, another clen — a re-encode, start over", vod399, vod399Redo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := resumeIdentityMismatch(&ResumeState{BaseURL: tc.saved}, "", tc.now)
			if got != tc.wantMismatch {
				t.Errorf("mismatch = %v (reason %q, identities %q / %q), want %v",
					got, reason, streamIdentity(tc.saved), streamIdentity(tc.now), tc.wantMismatch)
			}
		})
	}
}

// headedBody is n bytes of fill behind an ftyp box header, so the finished
// file passes validateDownloadedMP4 and each rendition's bytes are countable.
func headedBody(n int, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, n)
	copy(b, "\x00\x00\x00\x18ftypdash")
	return b
}

// TestDirectResumeRefusesADifferentTotal pins the probed-total half of the
// whole-file resume check. The sidecar records the total the partial was a
// prefix of; a resume whose own probe answers another total is a different
// file — here behind a URL with no fingerprint and no StreamID, the shape the
// identity check cannot see — and starts over instead of appending.
//
// Mutant: dropping `TotalSize: d.directTotalSize` from saveResume — the
// sidecar records no total and the resume splices. Mutant: dropping
// `d.directTotalSize = state.TotalSize` from Start — the same splice. Mutant:
// dropping the `total != d.directTotalSize` arm in differentFileReason — the
// same splice.
func TestDirectResumeRefusesADifferentTotal(t *testing.T) {
	bodyA := headedBody(2*DownloadChunkSize+100, 'A')
	bodyB := headedBody(3*DownloadChunkSize, 'B')
	out := filepath.Join(t.TempDir(), "video.mp4")

	// Run 1: rendition A, cut after its first chunk was checkpointed.
	full := serveRangeFile(bodyA)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		if start >= DownloadChunkSize {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		full(w, r)
	}))
	defer srvA.Close()
	d1 := NewSegmentDownloader(DownloaderOptions{BaseURL: srvA.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d1.delays = fastDelays()
	d1.directResumeIntervalOverride = DownloadChunkSize
	if err := d1.Start(t.Context()); err == nil {
		t.Fatal("run 1 Start = nil, want the 404 that cuts it")
	}
	var saved ResumeState
	if data, err := os.ReadFile(out + resumeFileSuffix); err != nil {
		t.Fatalf("run 1 left no sidecar: %v", err)
	} else if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.TotalSize != int64(len(bodyA)) || saved.BytesWritten != DownloadChunkSize {
		t.Fatalf("sidecar = %+v, want totalSize %d at %d bytes", saved, len(bodyA), DownloadChunkSize)
	}

	// Run 2: the same path now serves a file of another length.
	srvB := httptest.NewServer(serveRangeFile(bodyB))
	defer srvB.Close()
	d2 := NewSegmentDownloader(DownloaderOptions{BaseURL: srvB.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d2.delays = fastDelays()
	if err := d2.Start(t.Context()); err != nil {
		t.Fatalf("run 2 Start: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, bodyB) {
		t.Errorf("output is %d bytes with %d of rendition A — want rendition B alone (%d bytes), not A's checkpoint with B appended",
			len(got), bytes.Count(got, []byte("A")), len(bodyB))
	}
}

// TestDirectResumeRefusesAPartialLongerThanTheFile pins the legacy half: a
// sidecar written before the total was recorded cannot be compared, but a
// partial already longer than the probed file is still a different file.
// The loop never ran for it — the offset was past the total — so the old
// partial "completed" as it stood.
//
// Mutant: dropping the `staged > total` arm in differentFileReason — Start
// returns nil over rendition A's partial.
func TestDirectResumeRefusesAPartialLongerThanTheFile(t *testing.T) {
	staged := headedBody(3000, 'A')
	bodyB := headedBody(2000, 'B')
	out := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(out, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	store := utils.ResumeStore[ResumeState]{Path: out + resumeFileSuffix}
	if err := store.Save(ResumeState{BytesWritten: int64(len(staged)), Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(serveRangeFile(bodyB))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d.delays = fastDelays()
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bodyB) {
		t.Errorf("output is %d bytes with %d of the old partial — want the %d-byte file the origin serves",
			len(got), bytes.Count(got, []byte("A")), len(bodyB))
	}
}

// TestDifferentFileReason pins the rule both whole-file paths hold a resumed
// partial to. Nothing staged has nothing to refuse — that is also what bounds
// restartDirectFallback to one level, since its discard zeroes the counter
// before it re-enters the fallback.
//
// Mutant: dropping the `staged <= 0` case — the nothing-staged row returns a
// reason, and a fallback re-entered after a discard would restart forever.
func TestDifferentFileReason(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		staged, recorded, total int64
		want                    bool
	}{
		{"nothing staged", 0, 1000, 16, false},
		{"the recorded total", 8, 16, 16, false},
		{"another total than the recorded one", 8, 16, 32, true},
		{"no recorded total, the partial within the file", 8, 0, 16, false},
		{"no recorded total, the partial longer than the file", 8, 0, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &SegmentDownloader{directTotalSize: tc.recorded}
			if got := d.differentFileReason(tc.staged, tc.total, "the probe"); (got != "") != tc.want {
				t.Errorf("differentFileReason(%d, %d) with %d recorded = %q, want a reason: %v",
					tc.staged, tc.total, tc.recorded, got, tc.want)
			}
		})
	}
}

// seedWholeFileResume stages a whole-file checkpoint — the bytes and the
// sidecar Start validates — recording total as the length of the file they
// are a prefix of (0: a legacy sidecar that recorded none).
func seedWholeFileResume(t *testing.T, out string, staged []byte, total int64) {
	t.Helper()
	if err := os.WriteFile(out, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	store := utils.ResumeStore[ResumeState]{Path: out + resumeFileSuffix}
	if err := store.Save(ResumeState{BytesWritten: int64(len(staged)), TotalSize: total, Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
}

// TestDirectFallbackResumeRefusesADifferentTotal pins the probed-total check
// on the path a failed probe routes a resume into. The sidecar holds a
// checkpoint of rendition A and A's total; the probe answers 503 throughout,
// so the streaming fallback asks for the rest with a Range, and the 206 that
// comes back states rendition B's total. That is a different file: the
// partial is discarded and B streamed from byte 0, where the fallback used to
// append B's tail to A's checkpoint and return nil over the splice.
//
// The restart breaks off part-way, and the origin refuses the rest with a 503
// for the remainder of the run, so the run ends once the fallback's attempts
// are spent. The checkpoint it leaves must not carry A's total — it describes
// B's bytes now. A third run resumes B from it.
//
// Mutant: dropping the differentFileReason call from the fallback's 206 arm —
// run 2 returns nil over A's checkpoint with B's tail appended. Mutant:
// dropping `d.directTotalSize = 0` from discardStagedMedia — run 2's
// checkpoint holds B's bytes to A's total.
func TestDirectFallbackResumeRefusesADifferentTotal(t *testing.T) {
	const totalA = 250_000
	bodyB := headedBody(300_000, 'B')
	out := filepath.Join(t.TempDir(), "video.mp4")
	seedWholeFileResume(t, out, headedBody(100_000, 'A'), totalA)

	var cut atomic.Bool
	cut.Store(true)
	serveB := serveRangeFile(bodyB)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch rng := r.Header.Get("Range"); {
		case rng == "bytes=0-0":
			w.WriteHeader(http.StatusServiceUnavailable) // the probe fails throughout
		case rng == "" && cut.Load():
			// The restart from byte 0 breaks off after 200 KB.
			w.Header().Set("Content-Length", strconv.Itoa(len(bodyB)))
			w.WriteHeader(http.StatusOK)
			w.Write(bodyB[:200_000])
		case rng == "bytes=200000-" && cut.Load():
			w.WriteHeader(http.StatusServiceUnavailable) // and the rest is refused
		default:
			serveB(w, r)
		}
	}))
	defer srv.Close()
	run := func() error {
		d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
		d.delays = fastDelays()
		d.directResumeIntervalOverride = 64 << 10
		return d.Start(t.Context())
	}

	if err := run(); err == nil {
		got, _ := os.ReadFile(out)
		t.Fatalf("run 2 Start = nil over %d bytes holding %d of rendition A — want B from byte 0, cut by the break-off",
			len(got), bytes.Count(got, []byte("A")))
	}
	var saved ResumeState
	if data, err := os.ReadFile(out + resumeFileSuffix); err != nil {
		t.Fatalf("the restart left no checkpoint: %v", err)
	} else if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.BytesWritten <= 0 || saved.TotalSize == totalA {
		t.Errorf("the restart's checkpoint = %+v — want B's bytes, not held to A's total %d", saved, totalA)
	}

	cut.Store(false)
	if err := run(); err != nil {
		t.Fatalf("run 3 Start: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bodyB) {
		t.Errorf("output is %d bytes with %d of rendition A — want rendition B alone (%d bytes)",
			len(got), bytes.Count(got, []byte("A")), len(bodyB))
	}
}

// TestDirectFallbackResume416HoldsThePartialToItsFile pins the 416 arm of the
// same check. A 416 at the resume offset used to read as "the staged file is
// already complete" whatever any total said, so a resume whose probe failed
// finished a partial of a longer file — or of a different, shorter one — as
// the archive. The partial is complete only when the totals known agree it is
// the whole file: a stated total naming another file restarts it, and a known
// total the offset falls short of is a short origin — an error that keeps the
// checkpoint for a Resume whose probe settles it.
//
// Mutant: dropping the `total > 0 && total != offset` return — both
// short-origin rows return nil over the partial. Mutant: dropping the
// differentFileReason call from the 416 arm — the shorter-file row fails as a
// short origin instead of fetching the file. Mutant: `total != offset` →
// `total > 0` — the at-the-total row fails over a complete file.
func TestDirectFallbackResume416HoldsThePartialToItsFile(t *testing.T) {
	staged := headedBody(100_000, 'A')
	bodyB := headedBody(50_000, 'B')
	for _, tc := range []struct {
		name         string
		recorded     int64  // the sidecar's TotalSize
		contentRange string // the 416's, "" for none
		wantErr      bool
		want         []byte // the output file afterwards
	}{
		{"below the recorded total — a short origin", 250_000, "", true, staged},
		{"a stated total past the offset — a short origin", 0, "bytes */250000", true, staged},
		{"a stated total shorter than the partial — a different file, fetched", 0, "bytes */50000", false, bodyB},
		{"at the recorded total — complete", int64(len(staged)), "", false, staged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "video.mp4")
			seedWholeFileResume(t, out, staged, tc.recorded)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch rng := r.Header.Get("Range"); rng {
				case "bytes=0-0":
					w.WriteHeader(http.StatusServiceUnavailable) // the probe fails throughout
				case "":
					w.Header().Set("Content-Length", strconv.Itoa(len(bodyB)))
					w.WriteHeader(http.StatusOK)
					w.Write(bodyB)
				default:
					if tc.contentRange != "" {
						w.Header().Set("Content-Range", tc.contentRange)
					}
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				}
			}))
			defer srv.Close()

			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
			d.delays = fastDelays()
			err := d.Start(t.Context())
			if tc.wantErr != (err != nil) {
				t.Fatalf("Start = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "416") {
				t.Errorf("error = %q, want it to name the 416", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, tc.want) {
				t.Errorf("output is %d bytes (%d A, %d B), want %d bytes (%d A, %d B)",
					len(got), bytes.Count(got, []byte("A")), bytes.Count(got, []byte("B")),
					len(tc.want), bytes.Count(tc.want, []byte("A")), bytes.Count(tc.want, []byte("B")))
			}
			if _, statErr := os.Stat(out + resumeFileSuffix); (statErr == nil) != tc.wantErr {
				t.Errorf("sidecar present = %v, want %v — an error keeps the checkpoint, a finished file clears it",
					statErr == nil, tc.wantErr)
			}
		})
	}
}

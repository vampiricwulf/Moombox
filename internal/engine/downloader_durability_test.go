package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestSaveResumeFsyncsMediaFirst pins owner decision O-G: the sidecar (and
// the DB's last_*_seq behind it) are durable while the media bytes were
// never fsynced, so after a power loss the persisted position could LEAD the
// durable media and the next run appended after a torn or zero-filled tail.
//
// Mutant: dropping the syncMediaFile call — syncs is 0 and the persisted
// position can outrun the bytes it describes.
//
// Do not add t.Parallel(): mutates the package-level syncMediaFile seam (see
// the rule on SegmentTimeout in downloader.go).
func TestSaveResumeFsyncsMediaFirst(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video.ts")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	f.Write([]byte("mediabytes"))

	syncs := 0
	prev := syncMediaFile
	t.Cleanup(func() { syncMediaFile = prev })
	syncMediaFile = func(file *os.File) error { syncs++; return file.Sync() }

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(5)
	d.bytesWritten.Store(10)

	d.saveResume()
	if syncs != 1 {
		t.Fatalf("media syncs = %d, want 1 — saveResume must fsync the media first", syncs)
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
}

// TestSaveResumeSkipsSaveWhenFsyncFails pins the failure rule: a position
// that cannot be backed by durable bytes is not written at all, so the
// PREVIOUS sidecar — which does describe bytes on disk — stands. The capture
// itself continues: saveResume returns normally and the caller keeps
// downloading, because a failed fsync must never abort a live recording.
//
// Mutant: logging the error and saving anyway — the sidecar advances past
// data that may never reach the platter.
//
// Do not add t.Parallel(): mutates the package-level syncMediaFile seam.
func TestSaveResumeSkipsSaveWhenFsyncFails(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video.ts")
	f, _ := os.Create(out)
	t.Cleanup(func() { f.Close() })
	f.Write([]byte("mediabytes"))

	prev := syncMediaFile
	t.Cleanup(func() { syncMediaFile = prev })
	syncMediaFile = func(*os.File) error { return errors.New("disk is on fire") }

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(5)
	d.bytesWritten.Store(10)

	d.saveResume()
	if _, err := os.Stat(out + ".resume.json"); err == nil {
		t.Fatal("a sidecar was written although the media fsync failed")
	}
}

// TestSaveResumeSkipsZeroByteHunt pins ENGINE-13 (report #49): the
// first-segment hunt advances currentSeq up to 20 with nothing written, and
// persisting that position made every Resume hunt 20 further — and from the
// second attempt CurrentSeq exceeded maxEvictionHuntAdvance, silencing
// diagnoseEvictedStart.
//
// Mutant: dropping the bytesWritten guard — a sidecar appears with
// LastSeq > 0 and BytesWritten 0.
func TestSaveResumeSkipsZeroByteHunt(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video_stream")
	f, _ := os.Create(out)
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(12) // mid-hunt
	d.bytesWritten.Store(0)

	d.saveResume()
	if _, err := os.Stat(out + ".resume.json"); err == nil {
		t.Fatal("a sidecar was written for a hunt that has produced no bytes")
	}
}

// TestMediaFsyncMatchesResumeSaveCount is the call-count half of O-G: the
// fsync belongs to the SAVE, not to the segment. A live HLS loop writes a
// segment on every reload but saves the sidecar twice (the first advance and
// the deferred final save — see TestHlsResumeSaveIsRateLimited), so the two
// counts pin each other.
//
// Mutants, one per direction:
//   - fsyncing per written segment (or per playlist reload): syncs is 12+
//     against 2 saves, which is the cost O-G bounded at ~4/min.
//   - dropping the fsync from saveResume: syncs is 0 against 2 saves.
//
// Do not add t.Parallel(): mutates the package-level syncMediaFile seam.
func TestMediaFsyncMatchesResumeSaveCount(t *testing.T) {
	const liveReloads = 12
	srv := hlsAdvancingServer(t, liveReloads)

	var mu sync.Mutex
	var saves int
	var syncs int
	prev := syncMediaFile
	t.Cleanup(func() { syncMediaFile = prev })
	syncMediaFile = func(file *os.File) error {
		mu.Lock()
		syncs++
		mu.Unlock()
		return file.Sync()
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
		// Same reason as TestHlsResumeSaveIsRateLimited: keep the final
		// reload on the sequential path so the save count is the live loop's
		// own, not the VOD-parallel finish's extra unconditional save.
		StopOnGap: true,
	})
	d.delays = fastDelays()
	d.delays.hlsResumeSave = time.Hour
	d.onResumeSaved = func(int) {
		mu.Lock()
		saves++
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (EXT-X-ENDLIST finish)", err)
	}

	mu.Lock()
	gotSaves, gotSyncs := saves, syncs
	mu.Unlock()

	if gotSaves == 0 {
		t.Fatalf("no sidecar was saved across %d reloads — the test cannot pin the fsync count", liveReloads)
	}
	if gotSyncs != gotSaves {
		t.Fatalf("media syncs = %d across %d sidecar saves (%d reloads), want one sync per save",
			gotSyncs, gotSaves, liveReloads)
	}
}

// TestDirectDownloadCheckpointsFreshRunAndResumes pins the direct path's
// resume sidecar end to end. saveResume returned early unless currentSeq > 0
// and the whole-file path never advances currentSeq, so directResumeInterval
// had NEVER written a sidecar on a fresh VOD: an interrupted 40 GB download
// restarted from byte 0 however many hours it had banked.
//
// The run is interrupted at the second checkpoint, so the assertions cover
// both halves: the sidecar the fresh run wrote, and a second downloader that
// resumes from it with nothing pre-seeded (every other direct-resume row in
// this package seeds the sidecar by hand, which is exactly why the gap
// survived).
//
// Mutant: restoring the `currentSeq > 0` guard for the direct path — zero
// checkpoints, so the run is never interrupted and saves is 0.
func TestDirectDownloadCheckpointsFreshRunAndResumes(t *testing.T) {
	// Two whole chunks plus a short tail: with the interval set to one chunk
	// each chunk crosses it exactly once, so the checkpoints are deterministic.
	body := bytes.Repeat([]byte("0123456789ABCDEF"), (2*DownloadChunkSize+1024)/16)
	srv := httptest.NewServer(serveRangeFile(body))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "video.mp4")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var mu sync.Mutex
	var checkpoints []int64
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  out,
		IsDirectURL: true,
	})
	d.delays = fastDelays()
	d.directResumeIntervalOverride = DownloadChunkSize
	d.onResumeSaved = func(int) {
		mu.Lock()
		checkpoints = append(checkpoints, d.bytesWritten.Load())
		n := len(checkpoints)
		mu.Unlock()
		if n == 2 {
			cancel() // interrupt right after the second checkpoint
		}
	}

	if err := d.Start(ctx); err == nil {
		t.Fatal("Start returned nil, want the cancel that the second checkpoint triggered")
	}

	mu.Lock()
	got := append([]int64(nil), checkpoints...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("sidecar written %d times (offsets %v) over %d bytes at a %d-byte interval, want 2",
			len(got), got, len(body), DownloadChunkSize)
	}
	if want := int64(DownloadChunkSize); got[0] != want {
		t.Errorf("first checkpoint at %d bytes, want %d", got[0], want)
	}
	if want := int64(2 * DownloadChunkSize); got[1] != want {
		t.Errorf("second checkpoint at %d bytes, want %d", got[1], want)
	}

	state := readResumeSidecar(t, out+resumeFileSuffix)
	if state.BytesWritten != got[1] {
		t.Fatalf("sidecar BytesWritten = %d, want the second checkpoint %d", state.BytesWritten, got[1])
	}
	if info, err := os.Stat(out); err != nil || info.Size() < state.BytesWritten {
		t.Fatalf("staged file is %v bytes, want at least the persisted %d (err %v)", info, state.BytesWritten, err)
	}

	// The resumed run finds the sidecar the first run left — nothing seeded.
	resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer resumeCancel()
	d2 := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  out,
		IsDirectURL: true,
	})
	d2.delays = fastDelays()
	if err := d2.Start(resumeCtx); err != nil {
		t.Fatalf("resumed Start: %v", err)
	}
	final, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(final, body) {
		t.Fatalf("resumed output is %d bytes, want the %d-byte body intact", len(final), len(body))
	}
	if _, err := os.Stat(out + resumeFileSuffix); !os.IsNotExist(err) {
		t.Errorf("sidecar still present after completion, stat err = %v", err)
	}
}

// TestDirectFallbackCheckpointsMidStream pins the streaming fallback's half of
// the same gap: it wrote gigabytes without ever checkpointing, so an
// interrupted non-Range VOD lost everything it had staged. It now saves on the
// SAME directResumeInterval cadence as the chunked loop rather than a second
// one of its own.
//
// Mutant: removing the fallback's saveResume call — no sidecar exists at all
// when the transfer is interrupted.
func TestDirectFallbackCheckpointsMidStream(t *testing.T) {
	const interval = 64 << 10
	body := bytes.Repeat([]byte("MOOMBOX!"), interval) // 8 intervals
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// No Range support: the probe ladder exhausts and the caller
			// hands over to the streaming fallback.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("x"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		for off := 0; off < len(body); off += interval / 2 {
			end := min(off+interval/2, len(body))
			if _, err := w.Write(body[off:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "video.mp4")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var mu sync.Mutex
	var checkpoints []int64
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  out,
		IsDirectURL: true,
	})
	d.delays = fastDelays()
	d.directResumeIntervalOverride = interval
	d.onResumeSaved = func(int) {
		mu.Lock()
		checkpoints = append(checkpoints, d.bytesWritten.Load())
		n := len(checkpoints)
		mu.Unlock()
		if n == 2 {
			cancel() // interrupt right after the second checkpoint
		}
	}

	if err := d.Start(ctx); err == nil {
		t.Fatal("Start returned nil, want the cancel that the second checkpoint triggered")
	}

	mu.Lock()
	got := append([]int64(nil), checkpoints...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("fallback checkpointed %d times (offsets %v) over %d bytes at a %d-byte interval, want at least 2",
			len(got), got, len(body), interval)
	}
	second := got[1]
	if second < interval*2 {
		t.Errorf("second checkpoint at %d bytes, want at least two intervals (%d)", second, interval*2)
	}

	state := readResumeSidecar(t, out+resumeFileSuffix)
	if state.BytesWritten != second {
		t.Fatalf("sidecar BytesWritten = %d, want the second checkpoint %d", state.BytesWritten, second)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if info.Size() < state.BytesWritten {
		t.Fatalf("staged file is %d bytes, behind the persisted position %d", info.Size(), state.BytesWritten)
	}
}

// TestDirectCheckpointSpansTheFallbacksRequests pins that the checkpoint
// cadence counts from the last checkpoint, not from the start of each
// request. The streaming fallback asks again after a break, an outage, a
// refresh or a short 206, and each request restarted the count from its own
// offset: a transfer whose requests each moved less than an interval wrote
// no sidecar however much it staged, and when it ended in an error the next
// run fetched the file from byte 0. Each row's transfer breaks off part-way
// through every request and then meets a 404; whatever it staged, the
// sidecar must lag it by less than one interval.
//
//   - re-asks after breaks: three requests of 64 KB each against a 96 KB
//     interval;
//   - a hand-off from the chunked loop: one 5 MB chunk, a mid-download 200,
//     and a fallback request that breaks after 3 MB, against a 7.5 MB
//     interval — the fallback started its count afresh at the hand-off;
//   - a discard starts the count over: a resume whose Range is answered with
//     another file's total discards a 300 KB partial and streams the new file
//     from byte 0, breaking after 128 KB, against a 96 KB interval — a mark
//     left at the discarded partial's length holds the first checkpoint back.
//
// Mutant: restarting the mark at each request (`d.directCheckpoint =
// d.bytesWritten.Load()` after streamDirectOnce's status switch) — the first
// two rows write no sidecar. Mutant: restarting it as the fallback starts —
// the hand-off row does not. Mutant: dropping `d.directCheckpoint = 0` from
// discardStagedMedia — the discard row does not.
func TestDirectCheckpointSpansTheFallbacksRequests(t *testing.T) {
	const kb = 1 << 10
	for _, tc := range []struct {
		name     string
		body     []byte
		interval int64
		seed     int // bytes of another file to stage first (0: none)
		probeOK  bool
		// serve answers data request n (1-based), or reports false to let
		// the origin serve the file by Range.
		serve func(n int, body []byte, w http.ResponseWriter, r *http.Request) bool
	}{
		{
			"re-asks after breaks", headedBody(1024*kb, 'V'), 96 * kb, 0, false,
			func(n int, body []byte, w http.ResponseWriter, r *http.Request) bool {
				if n > 3 {
					w.WriteHeader(http.StatusNotFound)
					return true
				}
				breakAfter(w, r, body, 64*kb)
				return true
			},
		},
		{
			"a hand-off from the chunked loop", headedBody(2*DownloadChunkSize+100, 'V'), DownloadChunkSize + DownloadChunkSize/2, 0, true,
			func(n int, body []byte, w http.ResponseWriter, r *http.Request) bool {
				switch {
				case n == 1:
					return false // the first chunk
				case n == 2:
					w.Header().Set("Content-Length", "1") // the Range ignored
					w.WriteHeader(http.StatusOK)
					w.Write(body[:1])
				case n == 3:
					breakAfter(w, r, body, 3<<20)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
				return true
			},
		},
		{
			"a discard starts the count over", headedBody(2048*kb, 'V'), 96 * kb, 300 * kb, false,
			func(n int, body []byte, w http.ResponseWriter, r *http.Request) bool {
				switch n {
				case 1:
					return false // the resume Range, answered with this file's total
				case 2:
					breakAfter(w, r, body, 128*kb) // the restart from byte 0
				default:
					w.WriteHeader(http.StatusNotFound)
				}
				return true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := serveRangeFile(tc.body)
			var mu sync.Mutex
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=0-0" {
					if tc.probeOK {
						full(w, r)
					} else {
						w.WriteHeader(http.StatusInternalServerError)
					}
					return
				}
				mu.Lock()
				requests++
				n := requests
				mu.Unlock()
				if !tc.serve(n, tc.body, w, r) {
					full(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			out := filepath.Join(t.TempDir(), "video.mp4")
			if tc.seed > 0 {
				seedWholeFileResume(t, out, headedBody(tc.seed, 'A'), int64(len(tc.body))*2)
			}
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
			d.delays = fastDelays()
			d.directResumeIntervalOverride = tc.interval
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := d.Start(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Start = %v, want the 404 that ends the run", err)
			}
			info, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(out + resumeFileSuffix); err != nil {
				t.Fatalf("%d bytes staged at a %d-byte interval, and no sidecar describes them (%v)", info.Size(), tc.interval, err)
			}
			saved := readResumeSidecar(t, out+resumeFileSuffix).BytesWritten
			if saved <= 0 || saved > info.Size() || info.Size()-saved >= tc.interval {
				t.Errorf("the sidecar records %d of %d staged bytes, want it within one %d-byte interval of them", saved, info.Size(), tc.interval)
			}
		})
	}
}

// breakAfter answers a request's Range — a 206 for the rest of the file, or
// a 200 for all of it without one — with a body that breaks off after cut
// bytes.
func breakAfter(w http.ResponseWriter, r *http.Request, body []byte, cut int) {
	var start int
	if rng := r.Header.Get("Range"); rng != "" {
		fmt.Sscanf(rng, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
	}
	w.Write(body[start : start+cut])
}

// readResumeSidecar reads a sidecar the production code wrote. Deliberately a
// plain json.Unmarshal rather than loadResume: the point of the direct-path
// rows is what is ON DISK, not what the loader is willing to accept.
func readResumeSidecar(t *testing.T, path string) ResumeState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar %s: %v", path, err)
	}
	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse sidecar %s: %v (%s)", path, err, data)
	}
	return state
}

// TestDirectResumeIntervalIsFiftyMegabytes pins the production cadence the
// two rows above exercise through their override seam, so shrinking the
// constant cannot pass unnoticed.
//
// Mutant: shrinking directResumeInterval (say to DownloadChunkSize) — the two
// override-driven rows above would still pass while production checkpointed,
// and fsynced, ten times as often as owner decision O-G priced.
func TestDirectResumeIntervalIsFiftyMegabytes(t *testing.T) {
	if got, want := int64(directResumeInterval), int64(50<<20); got != want {
		t.Fatalf("directResumeInterval = %d bytes, want %d (50 MB)", got, want)
	}
}

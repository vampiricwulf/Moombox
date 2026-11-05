package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fullChunks is a chunkJitter that draws no jitter: every whole-file chunk is
// exactly DownloadChunkSize, for a test whose origin keys on that boundary
// (a cut at the second chunk's start, a checkpoint per chunk).
func fullChunks(n int64) int64 { return n - 1 }

// TestDirectRetryBackoffLadder pins the whole-file ladder's waits in
// production units: 1, 2, 4, 8, 16 s and then 30 s for each attempt after,
// so MaxChunkRetries' eleven attempts span 181 s. yt-dlp retries at once by
// default; Moombox backs off on purpose (directRetryBackoff).
//
// Mutant: directBackoffCapUnits 30 → 60 (the old cap) — the sixth wait is
// 32 s and the sum 303 s. Mutant: dropping the `n < 5` guard — a shift of 64
// or more is 0 in Go, so the n=64 row waits nothing at all.
func TestDirectRetryBackoffLadder(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "o.mp4")})
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30, 30, 30, 30, 30}
	var sum time.Duration
	for n, w := range want {
		if got := d.directRetryBackoff(n); got != w*time.Second {
			t.Errorf("directRetryBackoff(%d) = %v, want %v", n, got, w*time.Second)
		}
		if n < MaxChunkRetries-1 {
			sum += d.directRetryBackoff(n)
		}
	}
	if sum != 181*time.Second {
		t.Errorf("the eleven-attempt ladder waits %v in all, want 181s", sum)
	}
	if got := d.directRetryBackoff(-1); got != time.Second {
		t.Errorf("directRetryBackoff(-1) = %v, want the first wait (1s)", got)
	}
	if got := d.directRetryBackoff(64); got != 30*time.Second {
		t.Errorf("directRetryBackoff(64) = %v, want the cap", got)
	}
}

// stallUntilCancelled answers with headers and then nothing, until the client
// gives up on the request — the link-failure shape the idle deadline reads as
// a request with no complete answer.
func stallUntilCancelled(status int, header http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		<-r.Context().Done()
	}
}

// TestDirectChunkRidesOutTenFailedAttempts pins G17 on the chunked loop: a 5xx
// or a request with no complete answer is asked again ten times after the
// first — yt-dlp's --retries 10 (HttpFD's RetryManager, downloader/http.py,
// options.py) — on directRetryBackoff's ladder, and the eleventh failure
// ends the download with its own cause. The ladder used to be three attempts
// a second and two apart: a burst of fast 503s ended the job in three
// seconds where yt-dlp rides it out.
//
// It runs in a synctest bubble with PRODUCTION delays over
// serveEngineInMemory, so the waits are asserted to the second on the
// bubble's clock: 181 s of ladder for ten failures and for eleven (no wait
// follows the last), plus ten idle deadlines (SegmentTimeout) for the
// stalled rows.
//
// Mutant: MaxChunkRetries = 3 (the old budget) — both recovering rows fail
// after 3 attempts. Mutant: MaxChunkRetries = 10 (ten attempts, not ten
// retries) — the recovering rows fail on the tenth. Mutant:
// directBackoffCapUnits = 60 — every row waits 303 s, not 181 s.
func TestDirectChunkRidesOutTenFailedAttempts(t *testing.T) {
	body := headedBody(4096, 'V')
	idle := SegmentTimeout
	for _, tc := range []struct {
		name     string
		failures int32
		fail     http.HandlerFunc
		wantErr  string        // "" for a finished file
		wantWait time.Duration // on the bubble's clock
	}{
		{"ten 503s, then the chunk", 10, statusOnly(http.StatusServiceUnavailable), "", 181 * time.Second},
		{"eleven 503s — the ladder runs out on the last one's cause", 11, statusOnly(http.StatusServiceUnavailable),
			fmt.Sprintf("after %d attempts: HTTP 503", MaxChunkRetries), 181 * time.Second},
		{"ten stalled answers, then the chunk", 10,
			stallUntilCancelled(http.StatusPartialContent, http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body))}}),
			"", 181*time.Second + 10*idle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Int32
			serve := serveRangeFile(body)
			serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-0" && asked.Add(1) <= tc.failures {
					tc.fail(w, r)
					return
				}
				serve(w, r)
			}))
			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video.mp4")
				d := NewSegmentDownloader(DownloaderOptions{BaseURL: inMemoryOrigin + "/video.mp4", OutputFile: out, IsDirectURL: true})
				start := time.Now()
				err := d.Start(context.Background())
				waited := time.Since(start)
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("Start = %v after %d failed attempts, want the chunk fetched on attempt %d", err, tc.failures, tc.failures+1)
					}
					if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
						t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Start = %v, want an error naming %q", err, tc.wantErr)
				}
				if got, want := asked.Load(), min(tc.failures+1, MaxChunkRetries); got != want {
					t.Errorf("chunk requests = %d, want %d", got, want)
				}
				if waited != tc.wantWait {
					t.Errorf("the download waited %v on the bubble's clock, want %v (1+2+4+8+16+30x5 s of ladder)", waited, tc.wantWait)
				}
			})
		})
	}
}

// statusOnly answers every request with status and no body.
func statusOnly(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
}

// TestDirectFallbackRidesOutTenFailedAttempts pins G17 on the streaming
// fallback, whose budget is the chunked loop's: ten 5xx answers and an
// eleventh request that streams the file finish it, eleven end it, and the
// waits between them are directRetryBackoff's capped ladder. The fallback's
// waits used to double without a cap — harmless at three attempts, nine
// minutes for the tenth at eleven.
//
// Mutant: restoring `time.Duration(1<<uint(failures-1))*unit` in
// runDirectDownloadFallback — the ladder waits 1023 s, not 181 s. Mutant:
// MaxChunkRetries = 3 — the recovering row ends after three requests.
func TestDirectFallbackRidesOutTenFailedAttempts(t *testing.T) {
	body := headedBody(4096, 'V')
	for _, tc := range []struct {
		name     string
		failures int32
		wantErr  bool
	}{
		{"ten 503s, then the file", 10, false},
		{"eleven 503s — the ladder runs out", 11, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Int32
			serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if asked.Add(1) <= tc.failures {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.Write(body)
			}))
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "video.mp4")
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatalf("open output: %v", err)
				}
				t.Cleanup(func() { f.Close() })
				d := NewSegmentDownloader(DownloaderOptions{BaseURL: inMemoryOrigin + "/video.mp4", OutputFile: path, IsDirectURL: true})
				d.outputFile = f

				start := time.Now()
				err = d.runDirectDownloadFallback(context.Background())
				waited := time.Since(start)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
						t.Fatalf("runDirectDownloadFallback = %v, want the eleventh 503", err)
					}
				} else {
					if err != nil {
						t.Fatalf("runDirectDownloadFallback = %v, want the file streamed on request %d", err, tc.failures+1)
					}
					if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
						t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
					}
				}
				if got, want := asked.Load(), min(tc.failures+1, MaxChunkRetries); got != want {
					t.Errorf("requests = %d, want %d", got, want)
				}
				if waited != 181*time.Second {
					t.Errorf("the fallback waited %v on the bubble's clock, want 181s (1+2+4+8+16+30x5)", waited)
				}
			})
		})
	}
}

// shortOrigin serves body (len(body) bytes) behind a size probe that states
// total, the length a few bytes past what it will ever serve. A Range from
// len(body) on gets end — a 416 or an empty 206 — and every other Range the
// bytes it covers.
func shortOrigin(body []byte, total int64, end func(w http.ResponseWriter, from int64)) http.HandlerFunc {
	serve := serveRangeFile(body)
	return func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", total))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[:1])
			return
		}
		var from int64
		fmt.Sscanf(rng, "bytes=%d-", &from)
		if from >= int64(len(body)) {
			end(w, from)
			return
		}
		serve(w, r)
	}
}

// emptyPartial answers a Range from `from` with an empty 206 stating total.
func emptyPartial(total int64) func(w http.ResponseWriter, from int64) {
	return func(w http.ResponseWriter, from int64) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, total-1, total))
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusPartialContent)
	}
}

func notSatisfiable(contentRange string) func(w http.ResponseWriter, from int64) {
	return func(w http.ResponseWriter, from int64) {
		if contentRange != "" {
			w.Header().Set("Content-Range", contentRange)
		}
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}
}

// TestDirectEndToleranceCompletesAFewBytesShort pins G56 on the chunked loop:
// an origin that stops serving fewer than directEndTolerance (100) bytes
// short of the total its probe stated — a 416 or an empty 206 at that offset
// — has delivered the whole file, as yt-dlp reads a 416 whose length is
// within 100 bytes of the partial ("YouTube sometimes adds or removes a few
// bytes from the end of the file", downloader/http.py, youtube-dl #175).
// Start returns nil, clears the sidecar and says so at Info. A shortfall of
// 100 bytes is still the short origin TestDirectChunkedLoopShortOriginIsAnError
// pins, an error that keeps the sidecar.
//
// Mutant: directEndsWithinTolerance returning false — every completing row
// errors, as it did. Mutant: `short >= directEndTolerance` → `short >
// directEndTolerance` — the 100-byte rows complete. The final 100% progress
// callback reports the file's real length as its total, so the job's line
// does not read "4999950 of 5000000 bytes" at 100%. Mutant: dropping
// `totalSize = offset` from the 416 tolerance arm — the 416 rows' last
// callback keeps the probed total; from the empty-206 arm — that row's does.
func TestDirectEndToleranceCompletesAFewBytesShort(t *testing.T) {
	body := headedBody(300_000, 'V')
	n := int64(len(body))
	for _, tc := range []struct {
		name     string
		short    int64
		end      func(total int64) func(w http.ResponseWriter, from int64)
		complete bool
	}{
		{"a 416 one byte short", 1, func(int64) func(http.ResponseWriter, int64) { return notSatisfiable("") }, true},
		{"a 416 99 bytes short", 99, func(int64) func(http.ResponseWriter, int64) { return notSatisfiable("") }, true},
		{"a 416 100 bytes short — a short origin", 100, func(int64) func(http.ResponseWriter, int64) { return notSatisfiable("") }, false},
		{"an empty 206 99 bytes short", 99, emptyPartial, true},
		{"an empty 206 100 bytes short — a short origin", 100, emptyPartial, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total := n + tc.short
			srv := httptest.NewServer(shortOrigin(body, total, tc.end(total)))
			t.Cleanup(srv.Close)
			out := filepath.Join(t.TempDir(), "video.mp4")
			lg := &secretLogger{}
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true, Logger: lg})
			d.delays = directDelays()
			d.directResumeIntervalOverride = 64 << 10 // a sidecar exists before the end
			var progMu sync.Mutex
			var last DownloadProgress
			d.OnProgress = func(p DownloadProgress) {
				progMu.Lock()
				last = p
				progMu.Unlock()
			}
			err := d.Start(t.Context())
			_, statErr := os.Stat(out + resumeFileSuffix)
			if !tc.complete {
				if err == nil || !strings.Contains(err.Error(), "short of its probed size") {
					t.Fatalf("Start = %v, want the short-origin error", err)
				}
				if statErr != nil {
					t.Errorf("resume sidecar gone (stat err = %v); a short origin keeps it for a Resume", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start = %v, want the file complete %d bytes short of its probed %d", err, tc.short, total)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the %d served", len(got), n)
			}
			if !os.IsNotExist(statErr) {
				t.Errorf("resume sidecar still present (stat err = %v); a complete file clears it", statErr)
			}
			if hits := lg.containing("100-byte tolerance"); len(hits) != 1 || !strings.HasPrefix(hits[0], "INFO ") {
				t.Errorf("log lines naming the tolerance = %q, want one Info line", hits)
			}
			progMu.Lock()
			defer progMu.Unlock()
			if last.Percent != 100 || last.Bytes != n || last.TotalBytes != n {
				t.Errorf("last progress = %+v, want 100%% with Bytes = TotalBytes = %d (the file's real length)", last, n)
			}
		})
	}
}

// TestDirectFallbackEndToleranceCompletesAFewBytesShort pins the same reading
// on the streaming fallback, which a resume reaches when its size probe
// fails: the resume Range from the end of the partial is answered with a 416
// (naming no total, or the total it stated) or an empty 206, and the total
// the partial is held to — the sidecar's, or the one the answer states — is
// fewer than 100 bytes past it. Both used to be the short-origin error, so a
// resume of a file YouTube had trimmed by a few bytes failed every time.
//
// Mutant: dropping the tolerance from the fallback's 416 arm — both 416
// rows error. Mutant: dropping it from the empty-206 arm — that row errors.
func TestDirectFallbackEndToleranceCompletesAFewBytesShort(t *testing.T) {
	staged := headedBody(200_000, 'V')
	n := int64(len(staged))
	for _, tc := range []struct {
		name     string
		short    int64
		end      func(total int64) func(w http.ResponseWriter, from int64)
		complete bool
	}{
		{"a 416 naming no total, 50 bytes short of the sidecar's", 50, func(int64) func(http.ResponseWriter, int64) { return notSatisfiable("") }, true},
		{"a 416 stating the total, 50 bytes short", 50, func(total int64) func(http.ResponseWriter, int64) {
			return notSatisfiable(fmt.Sprintf("bytes */%d", total))
		}, true},
		{"an empty 206 stating the total, 50 bytes short", 50, emptyPartial, true},
		{"a 416 naming no total, 100 bytes short — a short origin", 100, func(int64) func(http.ResponseWriter, int64) { return notSatisfiable("") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total := n + tc.short
			out := filepath.Join(t.TempDir(), "video.mp4")
			seedWholeFileResume(t, out, staged, total)
			end := tc.end(total)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=0-0" {
					w.WriteHeader(http.StatusServiceUnavailable) // the probe fails: the fallback resumes
					return
				}
				var from int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from)
				end(w, from)
			}))
			t.Cleanup(srv.Close)
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
			d.delays = directDelays()
			err := d.Start(t.Context())
			_, statErr := os.Stat(out + resumeFileSuffix)
			if !tc.complete {
				if err == nil || !strings.Contains(err.Error(), "short of its probed size") {
					t.Fatalf("Start = %v, want the short-origin error", err)
				}
				if statErr != nil {
					t.Errorf("resume sidecar gone (stat err = %v); a short origin keeps it", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start = %v, want the partial complete %d bytes short of its total %d", err, tc.short, total)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, staged) {
				t.Errorf("output is %d bytes, want the %d staged", len(got), n)
			}
			if !os.IsNotExist(statErr) {
				t.Errorf("resume sidecar still present (stat err = %v); a complete file clears it", statErr)
			}
		})
	}
}

// TestDirectResumeKeepsThePartialWhenTheTotalMovedAFewBytes pins G56's
// resume half: a resume whose probe states a total fewer than 100 bytes from
// the one the sidecar was saved against is the same file, and the download
// continues from the partial to the new total. Any other move discarded a
// multi-GB partial and fetched the file again from byte 0
// (differentFileReason). 100 bytes or more is still a different file.
//
// Mutant: restoring `total != d.directTotalSize` in differentFileReason —
// both keeping rows refetch from byte 0. A kept partial is the one record
// that the total moved under it, so it is logged once at Info with both
// totals. Mutant: disabling that log line — the keeping rows find none.
func TestDirectResumeKeepsThePartialWhenTheTotalMovedAFewBytes(t *testing.T) {
	const savedTotal = 300_000
	const staged = 120_000
	for _, tc := range []struct {
		name string
		move int
		keep bool
	}{
		{"the file grew by 50 bytes", 50, true},
		{"the file shrank by 50 bytes", -50, true},
		{"the file grew by 100 bytes — a different file", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := headedBody(savedTotal+tc.move, 'V')
			for i := 16; i < len(body); i++ {
				body[i] = byte(i % 251)
			}
			out := filepath.Join(t.TempDir(), "video.mp4")
			seedWholeFileResume(t, out, body[:staged], savedTotal)
			var mu sync.Mutex
			var starts []int64
			serve := serveRangeFile(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rng := r.Header.Get("Range"); rng != "bytes=0-0" {
					var from int64
					fmt.Sscanf(rng, "bytes=%d-", &from)
					mu.Lock()
					starts = append(starts, from)
					mu.Unlock()
				}
				serve(w, r)
			}))
			t.Cleanup(srv.Close)
			lg := &secretLogger{}
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true, Logger: lg})
			d.delays = directDelays()
			if err := d.Start(t.Context()); err != nil {
				t.Fatalf("Start = %v", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Fatalf("output is %d bytes, want the %d-byte file intact", len(got), len(body))
			}
			hits := lg.containing("keeping the partial")
			if tc.keep {
				want := fmt.Sprint("savedTotal", savedTotal, "total", savedTotal+tc.move)
				if len(hits) != 1 || !strings.HasPrefix(hits[0], "INFO ") || !strings.Contains(hits[0], want) {
					t.Errorf("log lines keeping the partial = %q, want one Info line carrying %q", hits, want)
				}
			} else if len(hits) != 0 {
				t.Errorf("log lines keeping the partial = %q, want none for a different file", hits)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(starts) == 0 {
				t.Fatal("no chunk was requested")
			}
			if tc.keep && starts[0] != staged {
				t.Errorf("first chunk requested from byte %d, want the partial kept and continued from %d", starts[0], staged)
			}
			if !tc.keep && starts[0] != 0 {
				t.Errorf("first chunk requested from byte %d, want a different file fetched from byte 0", starts[0])
			}
		})
	}
}

// TestDirectChunksAreJittered pins G57: each chunk the whole-file loop asks
// for is drawn from 95% to 100% of DownloadChunkSize, both ends included —
// yt-dlp's random.randint(int(chunk_size * 0.95), chunk_size) per connection
// (downloader/http.py, youtube-dl b91a7a4e5) — where every request used to
// end exactly 5 MiB past the last. The seam's two extremes pin the bounds
// exactly; the production source is held to them over a real download; and
// the file arrives intact whatever the draw, since each chunk starts where
// the bytes written end.
//
// Mutant: `end := offset + DownloadChunkSize - 1` (no jitter) — the floor row
// asks for full chunks. Mutant: drawing from
// DownloadChunkSize-directChunkFloor (randint's upper end excluded) — the
// full row's chunks are one byte short and the seam sees n one smaller.
// Mutant: adding the draw to DownloadChunkSize (jitter upward) — every row's
// chunks overrun the bound.
func TestDirectChunksAreJittered(t *testing.T) {
	body := headedBody(3*DownloadChunkSize+100, 'V')
	for i := 16; i < len(body); i++ {
		body[i] = byte(i % 251)
	}
	span := int64(DownloadChunkSize - directChunkFloor + 1)
	for _, tc := range []struct {
		name   string
		jitter func(n int64) int64
		lo, hi int64 // bounds on every chunk but the last
	}{
		{"the seam's lowest draw: 95% chunks", func(int64) int64 { return 0 }, directChunkFloor, directChunkFloor},
		{"the seam's highest draw: full chunks", func(n int64) int64 { return n - 1 }, DownloadChunkSize, DownloadChunkSize},
		{"the production source", nil, directChunkFloor, DownloadChunkSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lengths []int64
			serve := serveRangeFile(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rng := r.Header.Get("Range"); rng != "bytes=0-0" {
					var from, to int64
					fmt.Sscanf(rng, "bytes=%d-%d", &from, &to)
					mu.Lock()
					lengths = append(lengths, to-from+1)
					mu.Unlock()
				}
				serve(w, r)
			}))
			t.Cleanup(srv.Close)
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
			d.delays = directDelays()
			var asked []int64
			if tc.jitter != nil {
				d.chunkJitter = func(n int64) int64 { asked = append(asked, n); return tc.jitter(n) }
			}
			if err := d.Start(t.Context()); err != nil {
				t.Fatalf("Start = %v", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Fatalf("output is %d bytes, want the %d-byte file intact", len(got), len(body))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(lengths) < 4 {
				t.Fatalf("chunk lengths = %v, want at least four chunks over %d bytes", lengths, len(body))
			}
			for i, l := range lengths[:len(lengths)-1] {
				if l < tc.lo || l > tc.hi {
					t.Errorf("chunk %d asked for %d bytes, want %d-%d", i, l, tc.lo, tc.hi)
				}
			}
			for _, n := range asked {
				if n != span {
					t.Errorf("the source was asked for a draw in [0, %d), want [0, %d) — randint includes both ends", n, span)
				}
			}
		})
	}
}

// TestDirectChunkJitterDrawsFromMathRand pins the seam's production value: a
// downloader whose chunkJitter is unset — every production downloader —
// draws from math/rand/v2's Int64N.
//
// Mutant: chunkJitterSource returning a constant function when the seam is
// nil — the pointer differs and the chunks stop varying.
func TestDirectChunkJitterDrawsFromMathRand(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "o.mp4")})
	if d.chunkJitter != nil {
		t.Fatal("NewSegmentDownloader set the chunkJitter test seam")
	}
	if got, want := reflect.ValueOf(d.chunkJitterSource()).Pointer(), reflect.ValueOf(rand.Int64N).Pointer(); got != want {
		t.Errorf("chunkJitterSource() is not math/rand/v2's Int64N")
	}
	if directChunkFloor != 4_980_736 {
		t.Errorf("directChunkFloor = %d, want int(5 MiB x 0.95) = 4980736", directChunkFloor)
	}
}

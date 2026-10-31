package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The tests in this file drive the real runDashLoop against an in-memory
// googlevideo — memGVS, served through serveEngineInMemory — inside synctest
// bubbles, so every wait the loop and its retry ladders sleep on (production
// values: no fastDelays here) passes on the bubble's clock, and a scenario
// that spans minutes of stream time runs in milliseconds with no wall-clock
// bound anywhere. They pin wave W3 of the 2026-10-31 yt-dlp audit: the
// manifest-free start's sq=0 (G3) and the init fetch (G4). They mutate the
// package-level engineHTTPClient, so none of them may call t.Parallel().

// gvsAnswer is what memGVS does with one request.
type gvsAnswer int

const (
	gvsServe       gvsAnswer = iota // 200 with gvsMedia(seq)
	gvsEmpty                        // 200 with no body
	gvsForbidden                    // 403
	gvsGone                         // 410
	gvsUnavailable                  // 503
)

// gvsInitPath is where memGVS serves a manifest Initialization (the
// non-manifest-free init shape); every other path is a segment, addressed by
// its sq query parameter.
const gvsInitPath = "/init"

// gvsProbeFloor separates the downloader's head probes (999999999, or
// currentSeq+1000) from real segment requests in memGVS's request log.
const gvsProbeFloor = 100_000

// gvsInit is the ftyp+moov a manifest-free stream carries only inline in its
// sq=0.
func gvsInit() []byte {
	return append(mp4box("ftyp", []byte("dashiso6avc1mp41")), mp4box("moov", []byte("init-boxes"))...)
}

// gvsFrag is sequence seq's media fragment: a bare moof+mdat, which FFmpeg
// cannot open without gvsInit ahead of it.
func gvsFrag(seq int) []byte {
	return append(mp4box("moof", fmt.Appendf(nil, "frag-%05d", seq)), mp4box("mdat", fmt.Appendf(nil, "media-%05d", seq))...)
}

// gvsMedia is the body memGVS serves for seq: sq=0 is the init followed by
// its own fragment, as googlevideo serves it; every later sequence is a bare
// fragment.
func gvsMedia(seq int) []byte {
	if seq == 0 {
		return append(gvsInit(), gvsFrag(0)...)
	}
	return gvsFrag(seq)
}

// gvsRecording is the file a downloader must leave when it recorded
// sequences from..to: a recording from sq=0 is those segments as served; one
// that began later is sq=0's init and then the later fragments.
func gvsRecording(from, to int) []byte {
	var b bytes.Buffer
	if from > 0 {
		b.Write(gvsInit())
	}
	for seq := from; seq <= to; seq++ {
		b.Write(gvsMedia(seq))
	}
	return b.Bytes()
}

// memGVS is an in-memory manifest-free googlevideo: query-style &sq=N
// addressing, X-Head-Seqnum on every response it answers while head reports
// one (head < 0 sends none, as an edge that has published nothing yet), and a
// per-sequence attempt count for scenarios that model transient failures.
type memGVS struct {
	head   func() int
	answer func(r *http.Request, seq, attempt int) gvsAnswer
	// onRequest, when set, runs for every segment request (not probes)
	// before it is answered.
	onRequest func(seq int)

	mu       sync.Mutex
	attempts map[int]int
	log      []int    // segment sequences requested, in order (probes excluded)
	sq0URLs  []string // the URL of every sq=0 request
}

func (g *memGVS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	seq := -1 // the manifest Initialization
	if r.URL.Path != gvsInitPath {
		n, err := strconv.Atoi(r.URL.Query().Get("sq"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seq = n
	}
	if h := g.head(); h >= 0 {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(h))
	}
	g.mu.Lock()
	g.attempts[seq]++
	attempt := g.attempts[seq]
	if seq < gvsProbeFloor {
		g.log = append(g.log, seq)
	}
	if seq == 0 {
		g.sq0URLs = append(g.sq0URLs, r.URL.String())
	}
	g.mu.Unlock()
	if g.onRequest != nil && seq < gvsProbeFloor {
		g.onRequest(seq)
	}
	switch g.answer(r, seq, attempt) {
	case gvsServe:
		body := gvsInit()
		if seq >= 0 {
			body = gvsMedia(seq)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	case gvsEmpty:
		w.WriteHeader(http.StatusOK)
	case gvsForbidden:
		w.WriteHeader(http.StatusForbidden)
	case gvsGone:
		w.WriteHeader(http.StatusGone)
	case gvsUnavailable:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// requested reports the segment sequences memGVS was asked for, in order.
func (g *memGVS) requested() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.log...)
}

func (g *memGVS) attemptsFor(seq int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts[seq]
}

func (g *memGVS) sq0Requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sq0URLs...)
}

// serveGVS installs a memGVS as the engine's HTTP client for the rest of the
// test. Call it outside the bubble, as serveEngineInMemory requires.
func serveGVS(t *testing.T, head func() int, answer func(r *http.Request, seq, attempt int) gvsAnswer) *memGVS {
	t.Helper()
	g := &memGVS{head: head, answer: answer, attempts: map[int]int{}}
	serveEngineInMemory(t, g)
	return g
}

func constHead(h int) func() int { return func() int { return h } }

// gvsBase is a manifest-free adaptiveFormats URL on the in-memory origin.
func gvsBase(id string) string {
	return inMemoryOrigin + "/videoplayback?id=" + id + ".1&itag=299&sig=stale"
}

func wantFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output = %d bytes starting %q, want %d bytes starting %q", len(got), head16(got), len(want), head16(want))
	}
}

func head16(b []byte) []byte { return b[:min(len(b), 16)] }

// TestDashHuntRetriesSq0AfterTheCipherSwap pins G3 (2026-10-31 audit,
// downloaders-1): a manifest-free capture that starts at sq=0 must never
// step past it, because sq=0 holds the stream's only ftyp+moov. Its first
// 403 fires OnCipherFailure, which repairs the URL — and the first-segment
// hunt then advanced to sq=1 anyway, below a head that said sq=0 existed,
// and wrote a file that began with a bare moof: hours of capture FFmpeg
// rejects with "moov atom not found". yt-dlp makes fragment 0 fatal and
// retries it (downloader/dash.py, fragment.py); so does huntMayAdvance now,
// and the recording is complete — sq=0's media as well as its init.
//
// Mutants:
//   - huntMayAdvance without its InitInlineAtStart rule: the hunt steps to
//     sq=1 below the known head and ensureInlineInit rescues the init, but
//     sq=0's own fragment is missing from the file.
//   - that rule and ensureInlineInit both gone (the code before the fix):
//     the file starts with moof.
func TestDashHuntRetriesSq0AfterTheCipherSwap(t *testing.T) {
	const head = 5
	g := serveGVS(t, constHead(head), func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == 0 && attempt == 1:
			return gvsForbidden // the stale n/sig the swap repairs
		case seq <= head:
			return gvsServe
		default:
			return gvsForbidden // past the end
		}
	})

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("hunt"),
			OutputFile:        out,
			InitInlineAtStart: true,
			CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
		})
		var swaps atomic.Int32
		d.OnCipherFailure = func() string {
			swaps.Add(1)
			return strings.Replace(gvsBase("hunt"), "sig=stale", "sig=fresh", 1)
		}

		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
		wantFile(t, out, gvsRecording(0, head))
		if got := swaps.Load(); got != 1 {
			t.Errorf("OnCipherFailure ran %d times, want 1", got)
		}
		if got := g.requested(); len(got) < 2 || got[0] != 0 || got[1] != 0 {
			t.Errorf("requests began %v, want sq=0 asked again after its 403", got[:min(len(got), 4)])
		}
	})
}

// TestDashHuntHoldsUntilTheHeadReachesIt pins step 3 of G3: the hunt may
// step past a refused sequence only below a KNOWN head. yt-dlp's live
// generator never requests a sequence X-Head-Seqnum has not reached
// (extractor/youtube/_video.py, _live_adaptive_fragments); a 403 with the head
// unknown, or at the head itself, is a segment not served yet — and stepping
// past it asked for later sequences that existed even less, twenty of them in
// two seconds, until the hunt gave up with nothing written or began the file
// past the start. Both rows run a plain downloader (no InitInlineAtStart), so
// the rule they pin is the general one.
//
// Mutants:
//   - huntMayAdvance always true (the code before the fix): both rows record
//     nothing, or begin after sq=0.
//   - huntMayAdvance advancing while the head is unknown: the go-live row
//     races past the edge before sq=0 is published.
//   - huntMayAdvance advancing whenever the head is known (cur <= head): the
//     at-the-head row steps off sq=0.
func TestDashHuntHoldsUntilTheHeadReachesIt(t *testing.T) {
	const last = 4 // the broadcast's last segment
	for _, tc := range []struct {
		name string
		// publishedAt is the head the edge reports elapsed into the run; -1
		// is nothing published (no X-Head-Seqnum at all).
		publishedAt func(elapsed time.Duration) int
		// refusals is how many times sq=0 is refused although published.
		refusals int
	}{
		{
			name: "go-live: nothing published, head unknown",
			publishedAt: func(e time.Duration) int {
				return min(int((e+250*time.Millisecond)/time.Second)-1, last)
			},
		},
		{
			name: "sq=0 is the head and refused twice",
			publishedAt: func(e time.Duration) int {
				return min(int(e/time.Second), last)
			},
			refusals: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var start atomic.Int64 // UnixNano on the bubble's clock
			elapsed := func() time.Duration { return time.Duration(time.Now().UnixNano() - start.Load()) }
			published := func() int { return tc.publishedAt(elapsed()) }
			serveGVS(t, published, func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case seq == 0 && attempt <= tc.refusals:
					return gvsForbidden
				case seq <= published():
					return gvsServe
				default:
					return gvsForbidden // not published yet, or past the end
				}
			})

			synctest.Test(t, func(t *testing.T) {
				start.Store(time.Now().UnixNano())
				out := filepath.Join(t.TempDir(), "video_stream")
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:    gvsBase("golive"),
					OutputFile: out,
					// Ended once the last segment has been out for a while.
					CheckStreamStatus: func(context.Context) (bool, error) {
						return elapsed() > time.Duration(last+2)*time.Second, nil
					},
				})
				if err := d.Start(t.Context()); err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
				wantFile(t, out, gvsRecording(0, last))
			})
		})
	}
}

// TestDashInitRetriesRefreshesAndTransientFailures pins G4 (downloaders-2): a
// part that force-starts past sq=0 (a quality split, restart discovery, the
// post-live refresh rebuild) takes its ftyp+moov from sq=0, and that fetch
// was one bare request whose failure was only logged — the part then recorded
// for hours into a file FFmpeg could not open. It now has
// fetchSegmentWithRetry's ladder (5xx, timeouts, outages) and,
// on a 403/410, initForbiddenAttempts tries each after refreshCredentials, on
// the sq=0 URL rebuilt from the base the refresh installed.
//
// Mutants:
//   - downloadInitSegment back to one fetchSegment, its failure logged: every
//     row records the fragments with no init ahead of them.
//   - fetchInitSegment without its 403/410 loop: the refused row fails Start.
//   - writeInitFromSq0 passing no rebuild closure: the refused row retries the
//     construction-time URL, refused again, and fails Start.
func TestDashInitRetriesRefreshesAndTransientFailures(t *testing.T) {
	const startSeq, head = 3, 6
	for _, tc := range []struct {
		name          string
		sq0           func(r *http.Request, attempt int) gvsAnswer
		wantRefreshes int
	}{
		{
			name: "refused on the stale URL, served on the refreshed base",
			sq0: func(r *http.Request, _ int) gvsAnswer {
				if r.URL.Query().Get("sig") == "stale" {
					return gvsForbidden
				}
				return gvsServe
			},
			wantRefreshes: 1,
		},
		{
			name: "503, then served",
			sq0: func(_ *http.Request, attempt int) gvsAnswer {
				if attempt == 1 {
					return gvsUnavailable
				}
				return gvsServe
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := serveGVS(t, constHead(head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case seq == 0:
					return tc.sq0(r, attempt)
				case seq <= head:
					return gvsServe
				default:
					return gvsForbidden
				}
			})

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				base := gvsBase("split")
				var refreshes atomic.Int32
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:         base,
					OutputFile:      out,
					StartSeq:        startSeq,
					ForceStartSeq:   true,
					InitURL:         base + "&sq=0",
					InitFromSegment: true,
					OnCredentialRefresh: func() (string, string) {
						refreshes.Add(1)
						return strings.Replace(base, "sig=stale", "sig=fresh", 1), ""
					},
					CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
				})
				if err := d.Start(t.Context()); err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
				wantFile(t, out, gvsRecording(startSeq, head))
				if got := int(refreshes.Load()); got != tc.wantRefreshes {
					t.Errorf("credential refreshes = %d, want %d", got, tc.wantRefreshes)
				}
				if tc.wantRefreshes > 0 {
					urls := g.sq0Requests()
					if last := urls[len(urls)-1]; !strings.Contains(last, "sig=fresh") {
						t.Errorf("the sq=0 that was served came from %q, want the refreshed base", last)
					}
				}
			})
		})
	}
}

// TestDashInitFailureEndsTheStartBeforeAnyMedia pins G4's other half: an init
// that cannot be had is fatal, as yt-dlp's fragment 0 is ("fragment … not
// found, unable to continue", downloader/fragment.py). Start returns
// ErrInitSegmentUnavailable before a single media request, leaving the part
// empty, so the orchestrator builds it again rather than record a file that
// cannot be muxed — and so the empty file cannot re-arm the first-segment hunt,
// which used to skip real segments at the split sequence. That holds for a
// manifest Initialization as for a manifest-free sq=0.
//
// Mutant: Start logging the failure and carrying on (the code before the
// fix): media is requested and written, and Start returns nil.
func TestDashInitFailureEndsTheStartBeforeAnyMedia(t *testing.T) {
	const startSeq, head = 3, 6
	for _, tc := range []struct {
		name         string
		manifestInit bool
		answer       gvsAnswer
		wantAttempts int
	}{
		{name: "manifest-free sq=0 gone", answer: gvsGone, wantAttempts: initForbiddenAttempts},
		{name: "manifest-free sq=0 refused", answer: gvsForbidden, wantAttempts: initForbiddenAttempts},
		{name: "manifest-free sq=0 unavailable", answer: gvsUnavailable, wantAttempts: MaxSegmentRetries},
		{name: "manifest Initialization refused", manifestInit: true, answer: gvsForbidden, wantAttempts: initForbiddenAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := serveGVS(t, constHead(head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case seq <= 0:
					return tc.answer
				case seq <= head:
					return gvsServe
				default:
					return gvsForbidden
				}
			})

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				base := gvsBase("fatal")
				opts := DownloaderOptions{
					BaseURL:         base,
					OutputFile:      out,
					StartSeq:        startSeq,
					ForceStartSeq:   true,
					InitURL:         base + "&sq=0",
					InitFromSegment: true,
					OnCredentialRefresh: func() (string, string) {
						return "", "fresh-token"
					},
					CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
				}
				initSeq := 0
				if tc.manifestInit {
					opts.InitURL = inMemoryOrigin + gvsInitPath + "?itag=299"
					opts.InitFromSegment = false
					initSeq = -1
				}
				d := NewSegmentDownloader(opts)

				err := d.Start(t.Context())
				if !errors.Is(err, ErrInitSegmentUnavailable) {
					t.Fatalf("Start = %v, want ErrInitSegmentUnavailable", err)
				}
				if info, statErr := os.Stat(out); statErr != nil || info.Size() != 0 {
					t.Errorf("output after a failed init: %v, size %v — want an empty file", statErr, info)
				}
				for _, seq := range g.requested() {
					if seq > 0 {
						t.Fatalf("media sequence %d was requested after the init failed (requests %v)", seq, g.requested())
					}
				}
				if got := g.attemptsFor(initSeq); got != tc.wantAttempts {
					t.Errorf("init fetched %d times, want %d", got, tc.wantAttempts)
				}
			})
		})
	}
}

// TestDashInlineInitGuardWritesSq0InitFirst pins step 2 of G3: an
// InitInlineAtStart file that is still empty never takes media for a
// sequence above 0 before sq=0's ftyp+moov (ensureInlineInit), on the
// sequential write and on catch-up's flush alike; when sq=0 cannot be had,
// the start fails instead. The route here is a resume sidecar naming a
// position past 0 with no bytes — older builds wrote them for a hunt that had
// written nothing, and loadResume still accepts one.
//
// Mutants:
//   - no ensureInlineInit on the sequential write: the "sequential" row's
//     file starts with moof.
//   - no ensureInlineInit on catch-up's flush: the "catch-up" row's does.
//   - ensureInlineInit logging a failure and returning nil: the "sq=0
//     refused" row writes media without an init and Start returns nil.
func TestDashInlineInitGuardWritesSq0InitFirst(t *testing.T) {
	const resumeAt = 5
	for _, tc := range []struct {
		name     string
		head     int
		sq0      gvsAnswer
		wantFail bool
	}{
		{name: "sequential", head: resumeAt + 4, sq0: gvsServe},
		{name: "catch-up", head: resumeAt + stayBehindSegments + CatchupThreshold + 10, sq0: gvsServe},
		{name: "sq=0 refused", head: resumeAt + 4, sq0: gvsForbidden, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveGVS(t, constHead(tc.head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case seq == 0:
					return tc.sq0
				case seq <= tc.head:
					return gvsServe
				default:
					return gvsForbidden
				}
			})

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				base := gvsBase("guard")
				if err := os.WriteFile(out, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				state, _ := json.Marshal(ResumeState{LastSeq: resumeAt - 1, BytesWritten: 0, Timestamp: time.Now().Unix(), BaseURL: base})
				if err := os.WriteFile(out+resumeFileSuffix, state, 0o644); err != nil {
					t.Fatal(err)
				}
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:           base,
					OutputFile:        out,
					InitInlineAtStart: true,
					CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
				})

				err := d.Start(t.Context())
				if tc.wantFail {
					if !errors.Is(err, ErrInitSegmentUnavailable) {
						t.Fatalf("Start = %v, want ErrInitSegmentUnavailable", err)
					}
					wantFile(t, out, nil)
					return
				}
				if err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
				wantFile(t, out, gvsRecording(resumeAt, tc.head))
			})
		})
	}
}

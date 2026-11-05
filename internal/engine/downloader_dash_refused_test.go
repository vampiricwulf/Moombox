package engine

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// The tests in this file pin wave W12 of the 2026-10-31 yt-dlp audit (G15,
// owner decision D8): a mid-stream DASH segment YouTube keeps refusing while
// later ones are served. They drive the real runDashLoop against memGVS inside
// synctest bubbles (see downloader_dash_integrity_test.go), so every retry
// sleep passes on the bubble's clock. They mutate the package-level
// engineHTTPClient, so none of them may call t.Parallel().

// refusedHole is the sequence the scenarios below refuse forever, and
// refusedHead the head they run to: well more than two past the hole, so
// moonarchive's `max_seq - 2 > cur_seq` holds, and close enough that the
// sequential loop — not catch-up — meets the hole.
const (
	refusedHole = 5
	refusedHead = 12
)

// gapLog collects a downloader's OnGap calls.
type gapLog struct {
	mu   sync.Mutex
	gaps []DownloadGap
}

func (l *gapLog) note(g DownloadGap) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gaps = append(l.gaps, g)
}

func (l *gapLog) all() []DownloadGap {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.gaps)
}

// refusedGVS serves a stream to refusedHead whose refusedHole answers hole
// forever; past the head every sequence is 403, the end of the stream. sq=0
// is served on its first request and answered sq0Later afterwards — the
// eviction probe's view of it.
func refusedGVS(t *testing.T, hole, sq0Later gvsAnswer) *memGVS {
	t.Helper()
	return serveGVS(t, constHead(refusedHead), func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == 0 && attempt > 1:
			return sq0Later
		case seq == refusedHole:
			return hole
		case seq <= refusedHead:
			return gvsServe
		default:
			return gvsForbidden
		}
	})
}

// TestDashRefusedSegmentClosesThePart pins D8's default: a manifest-free
// capture whose segment 5 answers 403 on every request, through a credential
// refresh, while 6 onward are served, stops AT 5 with a *SegmentRefusedError
// for the live loop to split on — the part closed with everything before the
// hole and nothing after it, its resume state kept, and no gap row of its own
// (the caller that moves past the hole records it). The stream is live
// throughout: this downloader used to answer the stuck segment with
// ErrQualityLost, which the live loop rebuilt at the same sequence for the
// rest of the broadcast.
//
// Mutants:
//   - runDashLoop without the refused-segment check: Start returns
//     ErrQualityLost.
//   - the check without its "a refresh was installed since the first
//     failure" condition: TestDashRefusedSegmentWaitsForFreshCredentials
//     splits with no refresh.
//   - refuseSegment skipping in place whatever sq=0 answers: Start goes on
//     past the hole and this test sees ErrQualityLost at the head (live).
func TestDashRefusedSegmentClosesThePart(t *testing.T) {
	g := refusedGVS(t, gvsForbidden, gvsServe)

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		var refreshes atomic.Int32
		var gaps gapLog
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("refused"),
			OutputFile:        out,
			InitInlineAtStart: true,
			OnCredentialRefresh: func() (string, string) {
				refreshes.Add(1)
				return "", "fresh-token"
			},
			CheckStreamStatus: func(context.Context) (bool, error) { return false, nil }, // live
		})
		d.SplitOnRefusedSegment = true
		d.OnGap = gaps.note

		err := d.Start(t.Context())
		var refused *SegmentRefusedError
		if !errors.As(err, &refused) || !errors.Is(err, ErrSegmentRefused) {
			t.Fatalf("Start = %v, want a *SegmentRefusedError", err)
		}
		if refused.Seq != refusedHole {
			t.Errorf("refused sequence = %d, want %d", refused.Seq, refusedHole)
		}
		if got := d.CurrentSeq(); got != refusedHole {
			t.Errorf("CurrentSeq = %d, want %d (stopped at the hole)", got, refusedHole)
		}
		wantFile(t, out, gvsRecording(0, refusedHole-1))
		if got := refreshes.Load(); got < 1 {
			t.Error("no credential refresh ran — the segment was given up on credentials it first failed with")
		}
		if got := gaps.all(); len(got) != 0 {
			t.Errorf("gap rows = %v, want none from the downloader (the split's caller records the hole)", got)
		}
		if got := g.attemptsFor(refusedHole + 1); got != 0 {
			t.Errorf("segment %d asked %d times, want 0 (the next part fetches it)", refusedHole+1, got)
		}
	})
}

// TestDashRefusedSegmentWaitsForFreshCredentials pins the rule's trigger as
// moonarchive has it (_dash.py, `last_seq_auth_expiry == cur_seq`): the 403
// must come AFTER fresh credentials were installed for the sequence. A
// refresh that installs nothing (the callback answers "" for both halves)
// leaves the segment with the credentials it first failed on, so the old
// answer stands — ErrQualityLost for the caller to rebuild.
func TestDashRefusedSegmentWaitsForFreshCredentials(t *testing.T) {
	refusedGVS(t, gvsForbidden, gvsServe)

	synctest.Test(t, func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:             gvsBase("norefresh"),
			OutputFile:          filepath.Join(t.TempDir(), "video_stream"),
			InitInlineAtStart:   true,
			OnCredentialRefresh: func() (string, string) { return "", "" },
			CheckStreamStatus:   func(context.Context) (bool, error) { return false, nil },
		})
		d.SplitOnRefusedSegment = true

		if err := d.Start(t.Context()); !errors.Is(err, ErrQualityLost) {
			t.Fatalf("Start = %v, want ErrQualityLost (no fresh credentials were ever installed)", err)
		}
	})
}

// TestDashRefusedSegmentRuleBounds pins what the rule never acts on, each row
// answering as it did before W12 — ErrQualityLost for a stream that is still
// live:
//   - a downloader whose caller cannot split (SplitOnRefusedSegment unset):
//     the post-live and VOD downloads, which have no parts;
//   - a hole within two of the head, moonarchive's `max_seq - 2 > cur_seq`:
//     it may not be served yet, or be one of the two phantom sequences an
//     ended broadcast's X-Head-Seqnum runs ahead by.
//
// Mutants: refusable without its SplitOnRefusedSegment term, or with the
// bound loosened to head-1 or head: the matching row returns a
// *SegmentRefusedError.
func TestDashRefusedSegmentRuleBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		split bool
		head  int
	}{
		{name: "caller cannot split", split: false, head: refusedHead},
		{name: "hole at head-2", split: true, head: refusedHole + 2},
		{name: "hole at head-1", split: true, head: refusedHole + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveGVS(t, constHead(tc.head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case seq == refusedHole:
					return gvsForbidden
				case seq <= tc.head:
					return gvsServe
				default:
					return gvsForbidden
				}
			})

			synctest.Test(t, func(t *testing.T) {
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:             gvsBase("bounds"),
					OutputFile:          filepath.Join(t.TempDir(), "video_stream"),
					InitInlineAtStart:   true,
					OnCredentialRefresh: func() (string, string) { return "", "fresh-token" },
					CheckStreamStatus:   func(context.Context) (bool, error) { return false, nil },
				})
				d.SplitOnRefusedSegment = tc.split

				if err := d.Start(t.Context()); !errors.Is(err, ErrQualityLost) {
					t.Fatalf("Start = %v, want ErrQualityLost", err)
				}
				if got := d.CurrentSeq(); got != refusedHole {
					t.Errorf("CurrentSeq = %d, want %d", got, refusedHole)
				}
			})
		})
	}
}

// TestDashRefusedSegmentSkipsInPlace pins D8's two exceptions, where the
// refused segment is skipped in place with a gap row — the upstreams' own
// answer (yt-dlp's skip_unavailable_fragments, moonarchive's "Skip it") —
// instead of closing the part:
//   - "sq=0 evicted": a manifest-free stream keeps its init only inline in
//     sq=0, and once sq=0 is no longer served a new part past the hole could
//     not open. The file goes on past the hole.
//   - "part holds no media": a part a split has just opened (forced start,
//     init taken from sq=0) whose very first segment is refused has nothing
//     to close; closing it would open one init-only part per refused
//     sequence. Its file begins with the init and goes on from the next
//     segment.
//
// Both download to the end of the stream and finish cleanly.
//
// Mutants:
//   - newPartInitEvicted answering false whatever sq=0 says: the first row
//     returns a *SegmentRefusedError.
//   - refuseSegment without its init-only rule: the second row does.
//   - refuseSegment advancing without OnGap: neither row records its gap.
//   - runDashLoop treating errSegmentSkipped as an error: Start returns it.
func TestDashRefusedSegmentSkipsInPlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start int // 0: from sq=0 (InitInlineAtStart); else a forced part with its init from sq=0
		sq0   gvsAnswer
		want  []byte
	}{
		{name: "sq=0 evicted", start: 0, sq0: gvsGone,
			want: append(gvsRecording(0, refusedHole-1), gvsFragments(refusedHole+1, refusedHead)...)},
		{name: "part holds no media", start: refusedHole, sq0: gvsServe,
			want: append(gvsInit(), gvsFragments(refusedHole+1, refusedHead)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedGVS(t, gvsForbidden, tc.sq0)

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				opts := DownloaderOptions{
					BaseURL:             gvsBase("skip"),
					OutputFile:          out,
					OnCredentialRefresh: func() (string, string) { return "", "fresh-token" },
					CheckStreamStatus:   func(context.Context) (bool, error) { return true, nil },
				}
				if tc.start > 0 {
					opts.StartSeq, opts.ForceStartSeq = tc.start, true
					opts.InitURL, opts.InitFromSegment = gvsBase("skip")+"&sq=0", true
				} else {
					opts.InitInlineAtStart = true
				}
				d := NewSegmentDownloader(opts)
				d.SplitOnRefusedSegment = true
				var gaps gapLog
				d.OnGap = gaps.note

				if err := d.Start(t.Context()); err != nil {
					t.Fatalf("Start = %v, want nil (the stream recorded to its end past the hole)", err)
				}
				wantFile(t, out, tc.want)
				if got, want := gaps.all(), []DownloadGap{{From: refusedHole, To: refusedHole}}; !slices.Equal(got, want) {
					t.Errorf("gap rows = %v, want %v", got, want)
				}
			})
		})
	}
}

// TestDashRebuildsExhaustedTakesTheSegment pins the second way into the rule:
// a 410, a 5xx or an empty body, which no in-loop refresh answers, reaches the
// live loop as ErrQualityLost, and each rebuild there is the fresh player
// response — the credential refresh — the rule waits for. Once the live loop
// has rebuilt at the sequence as often as it will (NoteRebuildsExhausted),
// the downloader that would hand the segment back once more takes it instead:
// it closes the part at it, or, with sq=0 no longer served, skips it in place
// and records the stream to its end. Unarmed, the same downloader still hands
// it back.
//
// Mutants:
//   - handBackForRebuild returning ErrQualityLost unconditionally: every
//     armed row fails.
//   - handleHTTPError's still-live branch returning ErrQualityLost directly:
//     the 503 and empty-body armed rows fail.
//   - handleGoneError's still-live verdict doing so: the 410 armed rows fail.
//   - handBackForRebuild ignoring which sequence was named: the "armed at
//     another sequence" row splits.
//   - runDashLoop treating errSegmentSkipped from handleDashError, or from the
//     empty-body handleHTTPError call, as an error: the matching "skipped"
//     row's Start returns it.
func TestDashRebuildsExhaustedTakesTheSegment(t *testing.T) {
	type outcome int
	const (
		handedBack outcome = iota
		split
		skipped
	)
	for _, tc := range []struct {
		name   string
		answer gvsAnswer
		sq0    gvsAnswer // sq=0 after its first request
		armAt  int       // -1: not armed
		want   outcome
	}{
		{name: "410 armed", answer: gvsGone, sq0: gvsServe, armAt: refusedHole, want: split},
		{name: "503 armed", answer: gvsUnavailable, sq0: gvsServe, armAt: refusedHole, want: split},
		{name: "empty body armed", answer: gvsEmpty, sq0: gvsServe, armAt: refusedHole, want: split},
		{name: "410 armed, sq=0 evicted", answer: gvsGone, sq0: gvsGone, armAt: refusedHole, want: skipped},
		{name: "503 armed, sq=0 evicted", answer: gvsUnavailable, sq0: gvsGone, armAt: refusedHole, want: skipped},
		{name: "empty body armed, sq=0 evicted", answer: gvsEmpty, sq0: gvsGone, armAt: refusedHole, want: skipped},
		{name: "410 not armed", answer: gvsGone, sq0: gvsServe, armAt: -1, want: handedBack},
		{name: "503 not armed", answer: gvsUnavailable, sq0: gvsServe, armAt: -1, want: handedBack},
		{name: "armed at another sequence", answer: gvsGone, sq0: gvsServe, armAt: refusedHole - 1, want: handedBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedGVS(t, tc.answer, tc.sq0)

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				var live atomic.Bool
				live.Store(true)
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:           gvsBase("armed"),
					OutputFile:        out,
					InitInlineAtStart: true,
					CheckStreamStatus: func(context.Context) (bool, error) { return !live.Load(), nil },
				})
				d.SplitOnRefusedSegment = true
				if tc.armAt >= 0 {
					d.NoteRebuildsExhausted(tc.armAt)
				}
				var gaps gapLog
				d.OnGap = gaps.note
				d.OnProgress = func(p DownloadProgress) {
					if p.Seq == refusedHead {
						live.Store(false) // the broadcast ends once its last segment is in
					}
				}

				err := d.Start(t.Context())
				switch tc.want {
				case handedBack:
					if !errors.Is(err, ErrQualityLost) {
						t.Fatalf("Start = %v, want ErrQualityLost", err)
					}
				case split:
					var refused *SegmentRefusedError
					if !errors.As(err, &refused) || refused.Seq != refusedHole {
						t.Fatalf("Start = %v, want a *SegmentRefusedError at %d", err, refusedHole)
					}
					wantFile(t, out, gvsRecording(0, refusedHole-1))
				case skipped:
					if err != nil {
						t.Fatalf("Start = %v, want nil (the stream recorded to its end past the hole)", err)
					}
					wantFile(t, out, append(gvsRecording(0, refusedHole-1), gvsFragments(refusedHole+1, refusedHead)...))
					if got, want := gaps.all(), []DownloadGap{{From: refusedHole, To: refusedHole}}; !slices.Equal(got, want) {
						t.Errorf("gap rows = %v, want %v", got, want)
					}
				}
			})
		})
	}
}

// TestRefusableTerms pins each term of refusable directly, sq=0 among them —
// a term no stream reaches through the loop (a file holding data never waits
// on sq=0), kept because sq=0 is yt-dlp's fatal fragment 0 and carries a
// manifest-free stream's only init.
//
// Mutants: dropping any term, or loosening the head bound: its row fails.
func TestRefusableTerms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		split bool
		bytes int64
		seq   int
		head  int64
		want  bool
	}{
		{name: "refusable", split: true, bytes: 1, seq: 5, head: 8, want: true},
		{name: "caller cannot split", split: false, bytes: 1, seq: 5, head: 8},
		{name: "nothing written", split: true, bytes: 0, seq: 5, head: 8},
		{name: "sq=0", split: true, bytes: 1, seq: 0, head: 8},
		{name: "head-2", split: true, bytes: 1, seq: 6, head: 8},
		{name: "head unknown", split: true, bytes: 1, seq: 5, head: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
			d.SplitOnRefusedSegment = tc.split
			d.bytesWritten.Store(tc.bytes)
			d.headSeq.Store(tc.head)
			if got := d.refusable(tc.seq); got != tc.want {
				t.Errorf("refusable(%d) = %v, want %v", tc.seq, got, tc.want)
			}
		})
	}
}

// TestNoteRebuildsExhaustedStartsUnset pins the constructor's default: a
// fresh downloader names no sequence, so sq=0 — a real sequence — is never
// read as armed.
func TestNoteRebuildsExhaustedStartsUnset(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	if got := d.RebuildsExhaustedSeq(); got != -1 {
		t.Errorf("rebuildsExhaustedSeq = %d, want -1", got)
	}
}

// gvsFragments is sequences from..to as memGVS serves them past sq=0: bare
// fragments, the media a file appends after its init.
func gvsFragments(from, to int) []byte {
	var b []byte
	for seq := from; seq <= to; seq++ {
		b = append(b, gvsFrag(seq)...)
	}
	return b
}

package engine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
	return serveGVS(t, constHead(refusedHead), refusedAnswers(hole, sq0Later, refusedHead))
}

// refusedAnswers is refusedGVS's answer function for a stream that runs to
// head.
func refusedAnswers(hole, sq0Later gvsAnswer, head int) func(r *http.Request, seq, attempt int) gvsAnswer {
	return func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == 0 && attempt > 1:
			return sq0Later
		case seq == refusedHole:
			return hole
		case seq <= head:
			return gvsServe
		default:
			return gvsForbidden
		}
	}
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
		if d.WholeStreamRefused() {
			t.Error("WholeStreamRefused = true for one segment refused while later ones are served")
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
// and records the stream to its end. A refusal (403, 410) is taken at once; a
// segment that only fails (503, an empty body) only once it has failed for
// unservedSegmentPersistence since the run of rebuilds began, so a googlevideo
// 5xx burst that outlasts the cap's three quick rebuilds is still waited out.
// It is handed back, as before, when it is unarmed, armed at another
// sequence, within two of the head, on a downloader whose caller cannot
// split, or when no later segment is served either.
//
// Mutants:
//   - handBackForRebuild returning ErrQualityLost unconditionally: every
//     armed row fails.
//   - handleHTTPError's still-live branch returning ErrQualityLost directly:
//     the 503 and empty-body armed rows fail.
//   - handleGoneError's still-live verdict doing so: the 410 armed rows fail.
//   - handBackForRebuild ignoring which sequence was named: the "armed at
//     another sequence" row splits.
//   - handBackForRebuild without its refusable gate: the "caller cannot
//     split" row splits.
//   - handBackForRebuild passing refuseSegment's errNotRefused on: the
//     "nothing later served" row returns it instead of ErrQualityLost.
//   - handBackForRebuild without its persistence bound for a segment that
//     only fails: the "not yet persisted" rows split.
//   - handleGoneError handing a 410 or a 403 back as a failure, not a
//     refusal: the fresh 410 and 403 armed rows wait out the bound instead.
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
	// persisted is a run of rebuilds that began long enough ago for a segment
	// that only fails (5xx, empty body) to be given up; fresh is one that
	// began just now, when only a refusal (403, 410) is.
	const (
		fresh     = time.Duration(0)
		persisted = unservedSegmentPersistence
	)
	for _, tc := range []struct {
		name    string
		answer  gvsAnswer
		sq0     gvsAnswer // sq=0 after its first request
		armAt   int       // -1: not armed
		age     time.Duration
		head    int  // 0: refusedHead
		unsplit bool // SplitOnRefusedSegment left unset
		blanket bool // every sequence from the hole on answers answer
		want    outcome
	}{
		{name: "410 armed", answer: gvsGone, sq0: gvsServe, armAt: refusedHole, want: split},
		{name: "403 armed", answer: gvsForbidden, sq0: gvsServe, armAt: refusedHole, want: split},
		{name: "503 armed", answer: gvsUnavailable, sq0: gvsServe, armAt: refusedHole, age: persisted, want: split},
		{name: "empty body armed", answer: gvsEmpty, sq0: gvsServe, armAt: refusedHole, age: persisted, want: split},
		{name: "503 armed, not yet persisted", answer: gvsUnavailable, sq0: gvsServe, armAt: refusedHole, age: fresh, want: handedBack},
		{name: "empty body armed, not yet persisted", answer: gvsEmpty, sq0: gvsServe, armAt: refusedHole, age: fresh, want: handedBack},
		{name: "410 armed, sq=0 evicted", answer: gvsGone, sq0: gvsGone, armAt: refusedHole, want: skipped},
		{name: "503 armed, sq=0 evicted", answer: gvsUnavailable, sq0: gvsGone, armAt: refusedHole, age: persisted, want: skipped},
		{name: "empty body armed, sq=0 evicted", answer: gvsEmpty, sq0: gvsGone, armAt: refusedHole, age: persisted, want: skipped},
		{name: "410 armed, hole at head-2", answer: gvsGone, sq0: gvsServe, armAt: refusedHole, head: refusedHole + 2, want: handedBack},
		{name: "410 armed, nothing later served", answer: gvsGone, sq0: gvsServe, armAt: refusedHole, blanket: true, want: handedBack},
		{name: "410 armed, caller cannot split", answer: gvsGone, sq0: gvsServe, armAt: refusedHole, unsplit: true, want: handedBack},
		{name: "410 not armed", answer: gvsGone, sq0: gvsServe, armAt: -1, want: handedBack},
		{name: "503 not armed", answer: gvsUnavailable, sq0: gvsServe, armAt: -1, age: persisted, want: handedBack},
		{name: "armed at another sequence", answer: gvsGone, sq0: gvsServe, armAt: refusedHole - 1, want: handedBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.head
			if head == 0 {
				head = refusedHead
			}
			answers := refusedAnswers(tc.answer, tc.sq0, head)
			if tc.blanket {
				answers = func(r *http.Request, seq, attempt int) gvsAnswer {
					if seq >= refusedHole {
						return tc.answer
					}
					return gvsServe
				}
			}
			serveGVS(t, constHead(head), answers)

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
				d.SplitOnRefusedSegment = !tc.unsplit
				if tc.armAt >= 0 {
					d.NoteRebuildsExhausted(tc.armAt, time.Now().Add(-tc.age))
				}
				var gaps gapLog
				d.OnGap = gaps.note
				d.OnProgress = func(p DownloadProgress) {
					if p.Seq == head {
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
					wantFile(t, out, append(gvsRecording(0, refusedHole-1), gvsFragments(refusedHole+1, head)...))
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

// TestDashRefusedSegmentNeedsALaterSegmentServed pins the check the rule
// makes before it gives anything up (laterSegmentServed): a refusal of EVERY
// segment from the hole on — a PO token or an address YouTube stopped
// accepting, through fresh credentials — is not one segment's refusal. The
// downloader hands the hole back as it did before W12 (ErrQualityLost for a
// live stream), with nothing skipped, no gap row and the part left open,
// where moonarchive's head bound alone would walk forward one dropped
// segment at a time.
//
// The decline is reported (WholeStreamRefused), so the live loop does not
// count the rebuild that follows toward its same-seq cap (owner decision
// PR-M2).
//
// Mutants: laterSegmentServed answering true whatever the probe says: Start
// returns a *SegmentRefusedError at the first refused sequence. runDashLoop
// not re-snapshotting the renewal count once the rule declined: the later
// segment is probed on every 403 of the hold. refuseSegment not latching
// wholeStreamRefused when it declines.
func TestDashRefusedSegmentNeedsALaterSegmentServed(t *testing.T) {
	g := serveGVS(t, constHead(refusedHead), func(r *http.Request, seq, attempt int) gvsAnswer {
		if seq >= refusedHole {
			return gvsForbidden
		}
		return gvsServe
	})

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		var refreshes atomic.Int32
		var gaps gapLog
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("blanket"),
			OutputFile:        out,
			InitInlineAtStart: true,
			OnCredentialRefresh: func() (string, string) {
				refreshes.Add(1)
				return "", "fresh-token"
			},
			CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
		})
		d.SplitOnRefusedSegment = true
		d.OnGap = gaps.note

		if err := d.Start(t.Context()); !errors.Is(err, ErrQualityLost) {
			t.Fatalf("Start = %v, want ErrQualityLost (every segment is refused, so none is given up)", err)
		}
		if got := refreshes.Load(); got < 1 {
			t.Fatal("no credential refresh ran — the rule was never reached")
		}
		if got := d.CurrentSeq(); got != refusedHole {
			t.Errorf("CurrentSeq = %d, want %d (held at the hole)", got, refusedHole)
		}
		if !d.WholeStreamRefused() {
			t.Error("WholeStreamRefused = false after the rule declined the hole")
		}
		wantFile(t, out, gvsRecording(0, refusedHole-1))
		if got := gaps.all(); len(got) != 0 {
			t.Errorf("gap rows = %v, want none", got)
		}
		// Declined, the rule is asked again only on the next fresh
		// credentials, so the later segment is probed at most once per
		// refresh — not on every 403 of the hold.
		if probes, refreshed := g.attemptsFor(refusedHead-1), int(refreshes.Load()); probes > refreshed {
			t.Errorf("later segment probed %d times across %d refreshes, want at most one probe per refresh", probes, refreshed)
		}
	})
}

// headByStatus stamps X-Head-Seqnum by the answer's status: served on a 2xx,
// refused on anything else — an edge whose refusals report a head no served
// segment ever has.
type headByStatus struct {
	http.ResponseWriter
	served, refused int
}

func (w *headByStatus) WriteHeader(code int) {
	h := w.served
	if code >= 300 {
		h = w.refused
	}
	w.Header().Set("X-Head-Seqnum", strconv.Itoa(h))
	w.ResponseWriter.WriteHeader(code)
}

// TestDashRefusedSegmentTrustsOnlyAServedHead pins the rule's head bound to
// the head SERVED segments report, as moonarchive's max_seq is (assigned only
// after raise_for_status()): here every served answer — the later segment
// refuseSegment probes among them — says the head is one past the hole,
// while the refusals and the head probes say it is far ahead. The hole is
// within two of the head the stream has demonstrably served, so it is handed
// back, not given up.
//
// Declined on the served head, the downloader reports it (WholeStreamRefused):
// the stall is at the edge the stream has served, which the live loop's
// same-seq cap must not count (owner decision PR-M2), though the tracked head
// says the stream stopped well behind it.
//
// Mutant: refuseSegment without its servedHeadSeq bound (the tracked head
// alone, as refusable reads it): Start returns a *SegmentRefusedError.
func TestDashRefusedSegmentTrustsOnlyAServedHead(t *testing.T) {
	g := &memGVS{head: constHead(-1), attempts: map[int]int{}, answer: func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == refusedHole:
			return gvsForbidden
		case seq < refusedHead:
			return gvsServe
		default:
			return gvsForbidden
		}
	}}
	serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.ServeHTTP(&headByStatus{ResponseWriter: w, served: refusedHole + 1, refused: refusedHead}, r)
	}))

	synctest.Test(t, func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:             gvsBase("servedhead"),
			OutputFile:          filepath.Join(t.TempDir(), "video_stream"),
			InitInlineAtStart:   true,
			OnCredentialRefresh: func() (string, string) { return "", "fresh-token" },
			CheckStreamStatus:   func(context.Context) (bool, error) { return false, nil },
		})
		d.SplitOnRefusedSegment = true

		err := d.Start(t.Context())
		if !errors.Is(err, ErrQualityLost) {
			t.Fatalf("Start = %v, want ErrQualityLost", err)
		}
		if got := d.HeadSeq(); got != refusedHead {
			t.Fatalf("headSeq = %d, want %d — the scenario needs the refusals' head tracked", got, refusedHead)
		}
		if !d.StoppedWellBehindHead() || !d.WholeStreamRefused() {
			t.Errorf("StoppedWellBehindHead = %v, WholeStreamRefused = %v; want both — the loop must read the served head's verdict, not the tracked head's",
				d.StoppedWellBehindHead(), d.WholeStreamRefused())
		}
	})
}

// TestDashRefusedSegmentSplitsUnlessSq0IsGone pins what "sq=0 evicted" means
// for D8's one exception: a 410, the answer fetchInitSegment also reads as
// gone for good. A 403 on sq=0 is the refusal the new part's own init ladder
// refreshes credentials for, and a 503 is transient, so either leaves D8's
// default standing — the part is closed at the hole, not skipped past it.
//
// Mutant: newPartInitEvicted reading any answer of 400 or above as evicted
// (ProbeSegmentAvailable's view): both rows skip in place instead.
func TestDashRefusedSegmentSplitsUnlessSq0IsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		sq0  gvsAnswer
	}{
		{name: "sq=0 refused", sq0: gvsForbidden},
		{name: "sq=0 unavailable", sq0: gvsUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedGVS(t, gvsForbidden, tc.sq0)

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				var gaps gapLog
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:             gvsBase("sq0"),
					OutputFile:          out,
					InitInlineAtStart:   true,
					OnCredentialRefresh: func() (string, string) { return "", "fresh-token" },
					CheckStreamStatus:   func(context.Context) (bool, error) { return false, nil },
				})
				d.SplitOnRefusedSegment = true
				d.OnGap = gaps.note

				var refused *SegmentRefusedError
				if err := d.Start(t.Context()); !errors.As(err, &refused) || refused.Seq != refusedHole {
					t.Fatalf("Start = %v, want a *SegmentRefusedError at %d", err, refusedHole)
				}
				wantFile(t, out, gvsRecording(0, refusedHole-1))
				if got := gaps.all(); len(got) != 0 {
					t.Errorf("gap rows = %v, want none", got)
				}
			})
		})
	}
}

// TestDashRefusedSegmentInitOnlyFileRebuiltInPlace pins holdsNoMedia's
// reading of the FILE: the live loop rebuilds a part in place — a fresh
// downloader appending to the same file at the same sequence — and a part a
// split has just opened holds its init and nothing else. The rebuilt
// downloader never wrote that init, so only the file says the part has no
// media to close. Armed at the hole after the cap's rebuilds, it skips the
// hole in place and records on, rather than closing a part FFmpeg could not
// mux.
//
// Mutant: holdsNoMedia answering false (the old "this Start wrote the init"
// rule, which a rebuild never meets): Start returns a *SegmentRefusedError
// over a file holding only its init.
func TestDashRefusedSegmentInitOnlyFileRebuiltInPlace(t *testing.T) {
	refusedGVS(t, gvsGone, gvsServe)

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		if err := os.WriteFile(out, gvsInit(), 0o644); err != nil {
			t.Fatal(err)
		}
		var live atomic.Bool
		live.Store(true)
		var gaps gapLog
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:         gvsBase("inplace"),
			OutputFile:      out,
			StartSeq:        refusedHole,
			ForceStartSeq:   true,
			InitURL:         gvsBase("inplace") + "&sq=0",
			InitFromSegment: true,
			CheckStreamStatus: func(context.Context) (bool, error) {
				return !live.Load(), nil
			},
		})
		d.SplitOnRefusedSegment = true
		d.NoteRebuildsExhausted(refusedHole, time.Now())
		d.OnGap = gaps.note
		d.OnProgress = func(p DownloadProgress) {
			if p.Seq == refusedHead {
				live.Store(false)
			}
		}

		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("Start = %v, want nil (the hole skipped in place, the stream recorded to its end)", err)
		}
		wantFile(t, out, append(gvsInit(), gvsFragments(refusedHole+1, refusedHead)...))
		if got, want := gaps.all(), []DownloadGap{{From: refusedHole, To: refusedHole}}; !slices.Equal(got, want) {
			t.Errorf("gap rows = %v, want %v", got, want)
		}
	})
}

// TestDashRefusedRunSplitsOnce pins a RUN of refused sequences — 5, 6 and 7
// answer 403 forever, 8 onward are served: one split, at the run's first
// sequence, and the part that split opens walks the rest of the run in one
// pass, as moonarchive does (`last_seq_auth_expiry = cur_seq` after a skip,
// "to avoid multiple player requests on sequential 403s"). Its first refusal
// waits for one credential refresh; every later one in the run meets the
// rule at once (afterSkip), so the second part needs exactly one refresh,
// records a gap row per sequence it steps over, and finishes the stream
// rather than handing a hole back for a rebuild — which would have closed a
// part holding nothing but its init.
//
// Mutant: afterSkip without its "already asked on fresh credentials" state:
// the second part refreshes again for each sequence of the run.
func TestDashRefusedRunSplitsOnce(t *testing.T) {
	const runEnd = refusedHole + 2
	serveGVS(t, constHead(refusedHead+8), func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq >= refusedHole && seq <= runEnd:
			return gvsForbidden
		case seq <= refusedHead+8:
			return gvsServe
		default:
			return gvsForbidden
		}
	})
	last := refusedHead + 8

	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		var live atomic.Bool
		live.Store(true)
		newPart := func(name string, start int, refreshes *atomic.Int32, gaps *gapLog) *SegmentDownloader {
			opts := DownloaderOptions{
				BaseURL:    gvsBase("run"),
				OutputFile: filepath.Join(dir, name),
				OnCredentialRefresh: func() (string, string) {
					refreshes.Add(1)
					return "", "fresh-token"
				},
				CheckStreamStatus: func(context.Context) (bool, error) { return !live.Load(), nil },
			}
			if start > 0 {
				opts.StartSeq, opts.ForceStartSeq = start, true
				opts.InitURL, opts.InitFromSegment = gvsBase("run")+"&sq=0", true
			} else {
				opts.InitInlineAtStart = true
			}
			d := NewSegmentDownloader(opts)
			d.SplitOnRefusedSegment = true
			d.OnGap = gaps.note
			d.OnProgress = func(p DownloadProgress) {
				if p.Seq == last {
					live.Store(false)
				}
			}
			return d
		}

		var r1, r2 atomic.Int32
		var g1, g2 gapLog
		d1 := newPart("part1", 0, &r1, &g1)
		var refused *SegmentRefusedError
		if err := d1.Start(t.Context()); !errors.As(err, &refused) || refused.Seq != refusedHole {
			t.Fatalf("first part: Start = %v, want a *SegmentRefusedError at %d", err, refusedHole)
		}

		// The live loop's split: the next part starts one past the hole.
		d2 := newPart("part2", refusedHole+1, &r2, &g2)
		if err := d2.Start(t.Context()); err != nil {
			t.Fatalf("second part: Start = %v, want nil (the rest of the run skipped, the stream recorded to its end)", err)
		}
		wantFile(t, filepath.Join(dir, "part2"), append(gvsInit(), gvsFragments(runEnd+1, last)...))
		var want []DownloadGap
		for seq := refusedHole + 1; seq <= runEnd; seq++ {
			want = append(want, DownloadGap{From: seq, To: seq})
		}
		if got := g2.all(); !slices.Equal(got, want) {
			t.Errorf("second part's gap rows = %v, want %v", got, want)
		}
		if got := r2.Load(); got != 1 {
			t.Errorf("second part ran %d credential refreshes, want 1 (the run after its first refusal is skipped at once)", got)
		}
	})
}

// TestStoppedWellBehindHead pins the accessor the live loop's rebuild cap
// counts by: more than two below the tracked head — which a refusal's header
// raises too, since a downloader rebuilt at a hole may stop before any
// segment is served to it.
func TestStoppedWellBehindHead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cur, head int64
		want      bool
	}{
		{name: "well behind", cur: 5, head: 8, want: true},
		{name: "head-2", cur: 6, head: 8},
		{name: "at the edge", cur: 9, head: 8},
		{name: "head unknown", cur: 5, head: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
			d.currentSeq.Store(tc.cur)
			d.headSeq.Store(tc.head)
			if got := d.StoppedWellBehindHead(); got != tc.want {
				t.Errorf("StoppedWellBehindHead = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDashSkippedSegmentLeavesFreshRetryState pins afterSkip's reset of the
// per-segment retry state: the gone burst spent on the hole is the hole's,
// not the next sequence's. Here the next sequence answers a short burst of
// 410s — fewer than goneRetryDuringDownload — before it is served; carried
// over, the hole's own burst (postBytes403CipherThreshold 403s before the
// refresh the rule waits for) pushed that burst past the threshold, to a
// stream-status check, and a live stream handed the sequence back for a
// rebuild (ErrQualityLost) instead of retrying it.
//
// Mutant: afterSkip without resetting consecutiveGoneErrors: Start returns
// ErrQualityLost.
func TestDashSkippedSegmentLeavesFreshRetryState(t *testing.T) {
	serveGVS(t, constHead(refusedHead), func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == 0 && attempt > 1:
			return gvsGone // sq=0 evicted: the hole is skipped in place
		case seq == refusedHole:
			return gvsForbidden
		case seq == refusedHole+1 && attempt <= goneRetryDuringDownload-postBytes403CipherThreshold+1:
			return gvsGone
		case seq <= refusedHead:
			return gvsServe
		default:
			return gvsForbidden
		}
	})

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		var live atomic.Bool
		live.Store(true)
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:             gvsBase("fresh"),
			OutputFile:          out,
			InitInlineAtStart:   true,
			OnCredentialRefresh: func() (string, string) { return "", "fresh-token" },
			CheckStreamStatus:   func(context.Context) (bool, error) { return !live.Load(), nil },
		})
		d.SplitOnRefusedSegment = true
		d.OnProgress = func(p DownloadProgress) {
			if p.Seq == refusedHead {
				live.Store(false)
			}
		}

		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("Start = %v, want nil (the hole skipped, the next sequence retried and recorded)", err)
		}
		wantFile(t, out, append(gvsRecording(0, refusedHole-1), gvsFragments(refusedHole+1, refusedHead)...))
	})
}

// WholeStreamRefused speaks for the hole the download stopped at, not for any
// decline in its life: the rule is also asked from inside the loop after a
// credential renewal, and a decline there followed by progress left the
// verdict latched, so a later, genuine single refused segment's stop reset
// the live loop's same-seq count instead of adding to it.
//
// Mutant: WholeStreamRefused reading a latch (any decline) instead of
// comparing the declined hole with CurrentSeq — the row that moved on reads
// true.
func TestWholeStreamRefusedIsTheStoppedHolesOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declined int // the hole the rule declined; -1 for none
		stopped  int
		want     bool
	}{
		{"no decline", -1, 50, false},
		{"stopped at the declined hole", 50, 50, true},
		{"declined at 0, stopped there", 0, 0, true},
		{"moved on past the declined hole", 50, 80, false},
	} {
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "video_stream")})
		if tc.declined >= 0 {
			d.wholeStreamRefusedAt.Store(int64(tc.declined) + 1)
		}
		d.currentSeq.Store(int64(tc.stopped))
		if got := d.WholeStreamRefused(); got != tc.want {
			t.Errorf("%s: WholeStreamRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

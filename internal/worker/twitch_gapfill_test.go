package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

var gapStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ms(d time.Duration) int64 { return gapStart.Add(d).UnixMilli() }

func covPart(idx int, from, to time.Duration, adBreak bool) stagedPartCoverage {
	return stagedPartCoverage{Index: idx, Cov: partCoverage{StartMs: ms(from), EndMs: ms(to), EndedAtAdBreak: adBreak}}
}

// TestGapSpansFor is the capture's half of owner decision D17: the opening is
// lost when the first frame is more than a minute after the broadcast's
// start, a hole between parts wider than gapFillMinGap is lost, a hole after
// a part closed at an ad break is an ad break's (kept only for a channel that
// fills them, follow-up 3), and the tail is lost when the broadcast ended
// while the capture was cut off. Parts arrive in any order.
//
// Mutants: the late-start threshold dropped (a 30 s join is lost); the
// minimum gap dropped (the 1 s seam of a lossless split is a span); the ad
// break kept without the opt-in; the tail left out; the gap <= 0 guard on an
// ad break's hole dropped (a zero-width span is planned).
func TestGapSpansFor(t *testing.T) {
	parts := []stagedPartCoverage{
		covPart(2, 50*time.Minute, 60*time.Minute, false),
		covPart(0, 5*time.Minute, 20*time.Minute, true),                        // closed at an ad break
		covPart(1, 22*time.Minute, 30*time.Minute+500*time.Millisecond, false), // after a 2 min ad break
		covPart(3, 60*time.Minute+time.Second, 70*time.Minute, false),          // a lossless 1 s seam
	}
	// Part 1 ends at 30:00.5 and part 2 starts at 50:00: a 20 min outage.
	got := gapSpansFor(gapStart, parts, true, true)
	want := []gapSpan{
		{FromMs: ms(0), ToMs: ms(5 * time.Minute), Kind: gapKindLateStart, AfterPart: -1},
		{FromMs: ms(20 * time.Minute), ToMs: ms(22 * time.Minute), Kind: gapKindAdBreak, AfterPart: 0},
		{FromMs: ms(30*time.Minute + 500*time.Millisecond), ToMs: ms(50 * time.Minute), Kind: gapKindLost, AfterPart: 1},
		{FromMs: ms(70 * time.Minute), Kind: gapKindTail, AfterPart: 3},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("spans =\n%+v\nwant\n%+v", got, want)
	}

	// Without the opt-in the ad break is not filled, and without an open
	// tail nothing follows the last part.
	got = gapSpansFor(gapStart, parts, false, false)
	for _, sp := range got {
		if sp.Kind == gapKindAdBreak || sp.Kind == gapKindTail {
			t.Errorf("span %+v, want no ad break and no tail", sp)
		}
	}
	if len(got) != 2 {
		t.Errorf("%d spans, want the late start and the outage", len(got))
	}

	// A part closed at an ad break whose next part picks up where it ended
	// (the break was stitched over no lost footage) has nothing to fill.
	seam := []stagedPartCoverage{covPart(0, 0, 10*time.Minute, true), covPart(1, 10*time.Minute, 20*time.Minute, false)}
	if got := gapSpansFor(gapStart, seam, true, false); len(got) != 0 {
		t.Errorf("a zero-width ad break has spans %+v", got)
	}

	// A join 30 s after the start is on time.
	if got := gapSpansFor(gapStart, []stagedPartCoverage{covPart(0, 30*time.Second, time.Hour, false)}, false, false); len(got) != 0 {
		t.Errorf("an on-time join has spans %+v", got)
	}
}

// TestCoverageRecorder: the first segment's start is written at once, the end
// at most coverageSaveEvery behind, and fully at close; a part resumed after
// a restart keeps what its file held; a discarded part forgets it.
//
// Mutants: the throttle compared against the start instead of the last save
// (nothing is written until 30 s in, so a crash before then leaves no start);
// close not writing (the end is stale); the load on resume removed (the
// resumed part's start becomes the restart's).
func TestCoverageRecorder(t *testing.T) {
	dir := t.TempDir()
	r := newCoverageRecorder(dir)
	r.note(gapStart, 2)
	if c := readPartCoverage(dir); c.StartMs != ms(0) {
		t.Fatalf("after the first segment: %+v, want the start on disk", c)
	}
	r.note(gapStart.Add(2*time.Second), 2)
	if c := readPartCoverage(dir); c.EndMs != ms(2*time.Second) {
		t.Errorf("4 s in: on-disk end %d, want the first save's (throttled)", c.EndMs)
	}
	r.close(true)
	if c := readPartCoverage(dir); c.EndMs != ms(4*time.Second) || !c.EndedAtAdBreak {
		t.Errorf("after close: %+v, want the end at 4 s and the ad-break mark", c)
	}

	resumed := newCoverageRecorder(dir)
	resumed.note(gapStart.Add(10*time.Second), 2)
	resumed.close(false)
	if c := readPartCoverage(dir); c.StartMs != ms(0) || c.EndMs != ms(12*time.Second) {
		t.Errorf("resumed part: %+v, want its original start and the new end", c)
	}
	resumed.reset()
	if _, err := os.Stat(filepath.Join(dir, twitchCoverageFile)); !os.IsNotExist(err) {
		t.Errorf("reset left the coverage file: %v", err)
	}
}

// TestVodSegmentRange is the timestamp mapping (owner decision D17): the
// segments that intersect the range, so the cut overlaps it rather than
// leave a hole, trimmed on segment boundaries.
//
// Mutant: segments that only touch the range kept (a 10 s overlap becomes
// 20 s); the half-open intersection made closed.
func TestVodSegmentRange(t *testing.T) {
	offsets := []float64{0, 10, 20, 30, 40}
	cases := []struct {
		from, to    float64
		first, last int
		ok          bool
	}{
		{12, 28, 1, 2, true},
		{10, 20, 1, 1, true}, // exactly one segment: its neighbours only touch it
		{0, 5, 0, 0, true},
		{35, 100, 3, 3, true},
		{40, 50, -1, -1, false}, // past the VOD's end
	}
	for _, c := range cases {
		first, last, ok := vodSegmentRange(offsets, c.from, c.to)
		if first != c.first || last != c.last || ok != c.ok {
			t.Errorf("vodSegmentRange(%v, %v) = %d, %d, %v; want %d, %d, %v", c.from, c.to, first, last, ok, c.first, c.last, c.ok)
		}
	}
}

// gapFillFixture is a Finished live job, its archive file, and a filler whose
// Twitch, HTTP and mux seams are in memory: the VOD's media playlist holds
// six 10 s segments, the third muted.
type gapFillFixture struct {
	db      *database.Database
	filler  *twitchGapFiller
	log     *captureLogger
	dir     string
	archive string
	served  map[string][]byte
	vodInfo twitch.TwitchVodInfo
	vodID   string
	endList bool
	failURL string
	// refuse answers a 403 for each URL in it, on every attempt.
	refuse  map[string]bool
	fetched int
}

func newGapFillFixture(t *testing.T) *gapFillFixture {
	t.Helper()
	f := &gapFillFixture{db: openTestDB(t), log: &captureLogger{}, dir: t.TempDir(), vodID: "555", endList: true}
	f.archive = filepath.Join(f.dir, "Streamer - Big day.mp4")
	if err := os.WriteFile(f.archive, []byte("RECORDED"), 0o644); err != nil {
		t.Fatal(err)
	}
	length, size := 3000, int64(8)
	if _, err := f.db.AddJob(&database.Job{ID: "tw_1", VideoID: "1", Platform: "twitch", Status: database.StatusFinished,
		OutputFile: f.archive, StreamStartTime: gapStart.Format(time.RFC3339), LengthSeconds: &length, FileSize: &size,
		TwitchQuality: "chunked", ChatFile: filepath.Join(f.dir, "Streamer - Big day.chat.json")}); err != nil {
		t.Fatal(err)
	}
	f.served = map[string][]byte{}
	for i := range 6 {
		name := fmt.Sprintf("%d.ts", i)
		if i == 2 {
			name = "2-muted.ts"
		}
		f.served["https://vod.example/chunked/"+name] = []byte(fmt.Sprintf("[v%d]", i))
	}
	f.filler = &twitchGapFiller{
		db: f.db, logger: f.log,
		findVod: func(context.Context, string, time.Time, time.Time) (string, error) { return f.vodID, nil },
		vodInfo: func(context.Context, string) (*twitch.TwitchVodInfo, error) { i := f.vodInfo; return &i, nil },
		vodVariants: func(context.Context, string) ([]twitch.TwitchHLSVariant, error) {
			return []twitch.TwitchHLSVariant{
				{Name: "720p60", URL: "https://vod.example/720p60/index-dvr.m3u8", Width: 1280, Height: 720, FPS: 60},
				{Name: "chunked", URL: "https://vod.example/chunked/index-dvr.m3u8", Width: 1920, Height: 1080, FPS: 60},
			}, nil
		},
		fetch: func(_ context.Context, url string) ([]byte, error) {
			f.fetched++
			if f.refuse[url] {
				return nil, &httpError{StatusCode: http.StatusForbidden}
			}
			if url == "https://vod.example/chunked/index-dvr.m3u8" {
				var b strings.Builder
				b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n")
				for i := range 6 {
					name := fmt.Sprintf("%d.ts", i)
					if i == 2 {
						name = "2-muted.ts"
					}
					fmt.Fprintf(&b, "#EXTINF:10.000,\n%s\n", name)
				}
				if f.endList {
					b.WriteString("#EXT-X-ENDLIST\n")
				}
				return []byte(b.String()), nil
			}
			if url == f.failURL {
				return nil, errors.New("refused")
			}
			if data, ok := f.served[url]; ok {
				return data, nil
			}
			return nil, fmt.Errorf("unexpected fetch %s", url)
		},
		mux: func(_ context.Context, ts, out string) error {
			data, err := os.ReadFile(ts)
			if err != nil {
				return err
			}
			return os.WriteFile(out, append([]byte("MP4"), data...), 0o644)
		},
	}
	return f
}

// plan saves a plan for tw_1 whose capture began 25 s into the broadcast's
// third VOD segment's span... see each test for the spans.
func (f *gapFillFixture) plan(t *testing.T, parts []stagedPartCoverage, spans []gapSpan, deadline time.Time) {
	t.Helper()
	p := &twitchGapFillPlan{JobID: "tw_1", Login: "streamer", StreamStart: gapStart.Format(time.RFC3339),
		EndedAt: gapStart.Add(time.Hour).Format(time.RFC3339), Deadline: deadline.Format(time.RFC3339),
		VariantName: "chunked", QualityPref: "720p60", Parts: parts, Spans: spans}
	if err := saveTwitchGapFillPlan(f.db, p, gapStart); err != nil {
		t.Fatal(err)
	}
}

func (f *gapFillFixture) row(t *testing.T) *twitchGapFillPlan {
	t.Helper()
	g, err := f.db.GetGapFill("tw_1")
	if err != nil || g == nil {
		return nil
	}
	var p twitchGapFillPlan
	if err := json.Unmarshal([]byte(g.Plan), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// TestGapFillInsertsTheLateStartAsAPart is owner decision D17 end to end on a
// single-file archive: the capture began at 25 s, the VOD is complete, and
// the opening is cut from the VOD on segment boundaries (the first three
// segments, the pad's overlap included), muxed beside the archive as its own
// part, and inserted before the recording — which becomes the job's second
// part, untouched, chat and all. The muted segment is used and logged; the
// plan is dropped.
//
// Mutants: the recovered part appended after the archive (time order lost);
// the archive's own row not synthesized (the job loses its recording); the
// muted log line removed; the variant matched by selection instead of name
// (the 720p60 rendition is cut).
func TestGapFillInsertsTheLateStartAsAPart(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if f.row(t) != nil {
		t.Error("the plan was kept after the fill")
	}
	job, _ := f.db.GetJob("tw_1")
	if len(job.Segments) != 2 {
		t.Fatalf("segments %+v, want the recovered part and the recording", job.Segments)
	}
	rec, orig := job.Segments[0], job.Segments[1]
	wantRec := filepath.Join(f.dir, "Streamer - Big day - recovered1.mp4")
	if rec.FilePath != wantRec || rec.SegmentIndex != 0 || rec.DurationSeconds != 30 || rec.Quality != "chunked" {
		t.Errorf("first part %+v, want the recovered opening (30 s of chunked)", rec)
	}
	if data, _ := os.ReadFile(wantRec); string(data) != "MP4[v0][v1][v2]" {
		t.Errorf("recovered part holds %q, want the first three VOD segments", data)
	}
	if orig.FilePath != f.archive || orig.SegmentIndex != 1 || orig.ChatFile == "" || orig.DurationSeconds != 3000 {
		t.Errorf("second part %+v, want the recording as it was", orig)
	}
	if data, _ := os.ReadFile(f.archive); string(data) != "RECORDED" {
		t.Errorf("the recording changed: %q", data)
	}
	if job.OutputFile != wantRec || job.LengthSeconds == nil || *job.LengthSeconds != 3030 {
		t.Errorf("job output_file %q, length %v; want the first part and the sum", job.OutputFile, job.LengthSeconds)
	}
	if _, logged := f.log.field("gap-fill: a recovered span uses muted VOD segments — its video is intact, its audio silent where Twitch muted it", "mutedSegments"); !logged {
		t.Error("the muted span was not logged")
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*.gapfill.ts")); len(matches) != 0 {
		t.Errorf("the cut's TS was left behind: %v", matches)
	}
}

// TestGapFillFillsBetweenRecordedParts: a span lost between two recorded
// parts is inserted between them, and the parts renumbered in time order. The
// cut overlaps the span by the pad on both sides: 20-40 s widened to 19-41 s
// takes the VOD's segments 1 through 4.
//
// Mutants: the recorded parts keyed by their row's unix_start instead of the
// plan's coverage (rows whose unix_start disagrees sort wrongly); the pad
// left off the span's start (segment 1 is not cut) or its end (segment 4).
func TestGapFillFillsBetweenRecordedParts(t *testing.T) {
	f := newGapFillFixture(t)
	p1 := filepath.Join(f.dir, "Streamer - Big day - part1.mp4")
	p2 := filepath.Join(f.dir, "Streamer - Big day - part2.mp4")
	for _, p := range []string{p1, p2} {
		os.WriteFile(p, []byte("RECORDED"), 0o644)
	}
	// Rows as a split job's finalize left them; unix_start is the session's
	// clock, not the footage's — part 2's here even reads earlier.
	if err := f.db.ReplaceJobSegments("tw_1", []database.Segment{
		{SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 20, UnixStart: 900},
		{SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 20, UnixStart: 100},
	}); err != nil {
		t.Fatal(err)
	}
	f.plan(t, []stagedPartCoverage{covPart(0, 0, 20*time.Second, false), covPart(1, 40*time.Second, 60*time.Second, false)},
		[]gapSpan{{FromMs: ms(20 * time.Second), ToMs: ms(40 * time.Second), Kind: gapKindLost, AfterPart: 0}},
		gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	job, _ := f.db.GetJob("tw_1")
	var names []string
	for _, s := range job.Segments {
		names = append(names, fmt.Sprintf("%d:%s", s.SegmentIndex, filepath.Base(s.FilePath)))
	}
	want := "0:Streamer - Big day - part1.mp4 1:Streamer - Big day - recovered1.mp4 2:Streamer - Big day - part2.mp4"
	if strings.Join(names, " ") != want {
		t.Errorf("parts %v, want %s", names, want)
	}
	if data, _ := os.ReadFile(filepath.Join(f.dir, "Streamer - Big day - recovered1.mp4")); string(data) != "MP4[v1][v2][v3][v4]" {
		t.Errorf("recovered part holds %q, want the VOD's segments 1-4 (the span and its pad)", data)
	}
}

// TestGapFillWaitsForTheCompleteVod (follow-up 4): a VOD that is not listed,
// still recording, or whose playlist has no ENDLIST is no fill — the plan is
// kept, its reason recorded and its next look put on the backoff — and the
// archive is left as it was.
//
// The output volume at disk_critical is a wait like these, before anything
// is fetched.
//
// Mutants: the IsRecording check removed (the fill runs on a growing VOD);
// the ENDLIST check removed (likewise); the backoff not applied (next look at
// once); the disk gate removed (the fill writes onto a full volume).
func TestGapFillWaitsForTheCompleteVod(t *testing.T) {
	for name, setup := range map[string]func(*gapFillFixture){
		"not listed":      func(f *gapFillFixture) { f.vodID = "" },
		"still recording": func(f *gapFillFixture) { f.vodInfo.IsRecording = true },
		"no ENDLIST":      func(f *gapFillFixture) { f.endList = false },
		"disk critical":   func(f *gapFillFixture) { f.filler.diskCritical = func() bool { return true } },
	} {
		t.Run(name, func(t *testing.T) {
			f := newGapFillFixture(t)
			setup(f)
			f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Hour, false)},
				[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
				gapStart.Add(25*time.Hour))
			row, _ := f.db.GetGapFill("tw_1")
			now := gapStart.Add(70 * time.Minute)
			f.filler.step(context.Background(), *row, now)

			p := f.row(t)
			if p == nil || p.Attempts != 1 || p.LastReason == "" {
				t.Fatalf("plan %+v, want it kept with one attempt and a reason", p)
			}
			g, _ := f.db.GetGapFill("tw_1")
			if g.NextCheckAt != now.Add(gapFillFirstCheck).Format(time.RFC3339) {
				t.Errorf("next look %s, want %s", g.NextCheckAt, now.Add(gapFillFirstCheck).Format(time.RFC3339))
			}
			if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 || job.OutputFile != f.archive {
				t.Errorf("the job changed: %+v", job)
			}
			if name == "disk critical" && f.fetched != 0 {
				t.Errorf("%d fetches on a critically full volume, want none", f.fetched)
			}
		})
	}
}

// TestGapFillNextLookIsCappedAtTheDeadline: a look whose backoff would land
// past the deadline is put one second after it instead, where the plan is
// dropped with its line, rather than outliving the window by up to three
// hours.
//
// Mutant: the cap removed (the next look is the full backoff away).
func TestGapFillNextLookIsCappedAtTheDeadline(t *testing.T) {
	f := newGapFillFixture(t)
	f.vodID = ""
	now := gapStart.Add(70 * time.Minute)
	deadline := now.Add(5 * time.Minute)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}}, deadline)
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, now)
	g, _ := f.db.GetGapFill("tw_1")
	if want := deadline.Add(time.Second).Format(time.RFC3339); g == nil || g.NextCheckAt != want {
		t.Errorf("next look %+v, want %s", g, want)
	}
}

// TestGapFillGivesUpAtTheDeadline (follow-up 4): past the deadline the plan
// is dropped with one line saying why, and the archive kept as recorded.
//
// Mutant: the deadline check removed (the fill runs, and adds a part).
func TestGapFillGivesUpAtTheDeadline(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Hour, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	p := f.row(t)
	p.LastReason = "the archive VOD is subscriber-only and this login has no access"
	saveTwitchGapFillPlan(f.db, p, gapStart)
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))

	if f.row(t) != nil {
		t.Error("the plan was kept past its deadline")
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 {
		t.Errorf("a part was added past the deadline: %+v", job.Segments)
	}
	if _, logged := f.log.field("gap-fill: keeping the archive as recorded — the archive VOD is subscriber-only and this login has no access within 24 hours of the broadcast's end", "jobID"); !logged {
		t.Errorf("no line saying why; logged %v", f.log.lines())
	}
}

// TestGapFillFailureLeavesTheArchive: a segment the VOD will not serve fails
// the fill — what it wrote is removed, the job and its recording untouched,
// and the plan kept for another look.
//
// Mutant: the undo removed (the half-made recovered part is left on disk).
func TestGapFillFailureLeavesTheArchive(t *testing.T) {
	f := newGapFillFixture(t)
	f.failURL = "https://vod.example/chunked/4.ts"
	f.plan(t, []stagedPartCoverage{covPart(0, 10*time.Second, 20*time.Second, false), covPart(1, 50*time.Second, time.Minute, false)},
		[]gapSpan{
			{FromMs: ms(0), ToMs: ms(10 * time.Second), Kind: gapKindLateStart, AfterPart: -1},
			{FromMs: ms(20 * time.Second), ToMs: ms(50 * time.Second), Kind: gapKindLost, AfterPart: 0},
		}, gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if p := f.row(t); p == nil || !strings.Contains(p.LastReason, "could not be cut") {
		t.Errorf("plan %+v, want it kept with the cut's failure", p)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the failed fill left %v", matches)
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 || job.OutputFile != f.archive {
		t.Errorf("the job changed: %+v", job)
	}
}

// TestGapFillBackoff pins the look schedule: 10 min doubling to three hours.
//
// Mutant: no cap (the sixth look waits 320 min).
func TestGapFillBackoff(t *testing.T) {
	want := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 3 * time.Hour, 3 * time.Hour}
	for i, w := range want {
		if got := gapFillBackoff(i + 1); got != w {
			t.Errorf("gapFillBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if gapFillWindow != 24*time.Hour {
		t.Errorf("window %v, want the owner's 24 h", gapFillWindow)
	}
}

// TestNewTwitchGapFillPlan: a capture with spans gets a plan carrying its
// variant's selection and a 24 h deadline; one that lacks nothing, or has no
// channel login (not a live capture of a channel), gets none.
//
// Mutant: the login guard removed (a plan the fill can never place).
func TestNewTwitchGapFillPlan(t *testing.T) {
	job := &database.Job{ID: "tw_1", StreamStartTime: gapStart.Format(time.RFC3339)}
	v := &TwitchVariantInfo{Name: "chunked", QualityPref: "1080p60", ChannelLogin: "streamer"}
	late := []stagedPartCoverage{covPart(0, 5*time.Minute, time.Hour, false)}
	now := gapStart.Add(time.Hour)
	p := newTwitchGapFillPlan(job, v, late, false, now)
	if p == nil || p.VariantName != "chunked" || p.QualityPref != "1080p60" || p.Deadline != now.Add(24*time.Hour).Format(time.RFC3339) || len(p.Spans) != 1 {
		t.Fatalf("plan %+v", p)
	}
	if newTwitchGapFillPlan(job, v, []stagedPartCoverage{covPart(0, 10*time.Second, time.Hour, false)}, false, now) != nil {
		t.Error("a capture that lacks nothing got a plan")
	}
	if newTwitchGapFillPlan(job, &TwitchVariantInfo{Name: "chunked"}, late, false, now) != nil {
		t.Error("a capture without a channel login got a plan")
	}
}

// TestNewTwitchGapFillerProductionSeams pins the production filler: every
// seam wired from the Twitch service, the orchestrator's muxer and the
// worker's HTTP client, whose fetch returns a 200's body and refuses any
// other status.
//
// Mutants: a seam left nil (fill reports no Twitch service, forever); the
// status check removed from gapFillFetch (a 404 page cut into a part).
func TestNewTwitchGapFillerProductionSeams(t *testing.T) {
	w, db := testWorkerSetup(t)
	f := newTwitchGapFiller(db, twitch.NewService(nil, discardLogger{}), w.orchestrator, discardLogger{})
	if f.findVod == nil || f.vodInfo == nil || f.vodVariants == nil || f.fetch == nil || f.mux == nil {
		t.Fatalf("a production seam is nil: %+v", f)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			http.NotFound(rw, r)
			return
		}
		rw.Write([]byte("segment"))
	}))
	t.Cleanup(srv.Close)
	if data, err := gapFillFetch(context.Background(), srv.URL+"/ok"); err != nil || string(data) != "segment" {
		t.Errorf("fetch = %q, %v", data, err)
	}
	if _, err := gapFillFetch(context.Background(), srv.URL+"/gone"); err == nil {
		t.Error("a 404 was fetched as a segment")
	}
	if twitchChannelGapFillAdBreaks(nil, "x") {
		t.Error("an unconfigured channel fills its ad breaks")
	}
}

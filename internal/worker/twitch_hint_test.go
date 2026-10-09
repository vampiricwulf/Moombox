package worker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

func TestTwitchHintCacheStashAndTake(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "111", IsLive: true}

	c.stash("tw_111", info)

	got := c.take("tw_111")
	if got == nil {
		t.Fatal("take after stash: want non-nil")
	}
	if got.StreamID != "111" {
		t.Errorf("StreamID: want 111, got %q", got.StreamID)
	}

	// take again — should be empty (take-once)
	if got := c.take("tw_111"); got != nil {
		t.Errorf("second take: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTakeMissing(t *testing.T) {
	c := newTwitchHintCache()
	if got := c.take("tw_does_not_exist"); got != nil {
		t.Errorf("take of unknown jobID: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTTLEviction(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "222", IsLive: true}

	// Stash with a manual past timestamp older than the TTL
	c.mu.Lock()
	c.entries["tw_222"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()

	if got := c.take("tw_222"); got != nil {
		t.Errorf("take of expired entry: want nil, got %+v", got)
	}

	// Verify the expired entry was removed (no zombie)
	c.mu.Lock()
	_, exists := c.entries["tw_222"]
	c.mu.Unlock()
	if exists {
		t.Error("expired entry should be removed from cache after take")
	}
}

func TestTwitchHintCacheNilSafe(t *testing.T) {
	var c *twitchHintCache
	// nil receiver should be safe (no-op)
	c.stash("tw_nil", &twitch.TwitchStreamInfo{StreamID: "nil"})
	if got := c.take("tw_nil"); got != nil {
		t.Errorf("take on nil cache: want nil, got %+v", got)
	}
	if s := c.Stats(); s.Hits != 0 || s.Misses != 0 {
		t.Errorf("Stats on nil cache: want zero, got %+v", s)
	}
}

func TestTwitchHintCacheStatsCounter(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "stat", IsLive: true}

	// 1 hit: stash + take
	c.stash("tw_stat", info)
	if got := c.take("tw_stat"); got == nil {
		t.Fatal("first take after stash: want non-nil")
	}

	// 2 misses: take of unknown ID twice
	c.take("tw_unknown_a")
	c.take("tw_unknown_b")

	// 1 miss via expiry: backdate then take
	c.mu.Lock()
	c.entries["tw_expired"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()
	if got := c.take("tw_expired"); got != nil {
		t.Fatal("expired take: want nil")
	}

	// 1 more hit: fresh stash + take
	c.stash("tw_stat2", info)
	if got := c.take("tw_stat2"); got == nil {
		t.Fatal("second take after stash: want non-nil")
	}

	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("Hits: want 2, got %d", s.Hits)
	}
	if s.Misses != 3 {
		t.Errorf("Misses: want 3, got %d", s.Misses)
	}
}

func TestTwitchHintCacheOverwriteOnDoubleStash(t *testing.T) {
	c := newTwitchHintCache()
	first := &twitch.TwitchStreamInfo{StreamID: "v1", IsLive: true}
	second := &twitch.TwitchStreamInfo{StreamID: "v2", IsLive: true}

	c.stash("tw_overwrite", first)
	c.stash("tw_overwrite", second)

	got := c.take("tw_overwrite")
	if got == nil {
		t.Fatal("take after double stash: want non-nil")
	}
	if got.StreamID != "v2" {
		t.Errorf("StreamID: want v2 (last write wins), got %q", got.StreamID)
	}
}

// TestProcessorStashAndTake verifies a stashed hint short-circuits the
// GetStreamInfo call. We can't easily inject a fake Twitch service without
// substantially refactoring StreamProcessor, so this test focuses on the
// observable side-effect: after stashing and a single take, the cache is
// empty (take-once). The end-to-end path is covered by the integration
// scenario in Phase 10.
func TestProcessorStashAndTake(t *testing.T) {
	sp := &StreamProcessor{twitchHints: newTwitchHintCache()}
	info := &twitch.TwitchStreamInfo{StreamID: "abc", IsLive: true, ChannelLogin: "AbcStreamer"}

	sp.StashTwitchStreamInfo(info)

	// The key is the channel, case-folded: the producer reads it off the info
	// it fetched and the consumer off the login it is waiting on, and those
	// two spellings are not guaranteed to match.
	got := sp.twitchHints.take(twitchHintKey("abcstreamer"))
	if got == nil || got.StreamID != "abc" {
		t.Fatalf("expected stashed info to be retrievable, got %+v", got)
	}

	// take-once: second take is empty
	if got := sp.twitchHints.take(twitchHintKey("abcstreamer")); got != nil {
		t.Errorf("second take after stash: want nil, got %+v", got)
	}

	// A hint with no channel on it cannot be keyed and must not be stashed
	// under an empty string, where every login-less take would find it.
	sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{StreamID: "nologin", IsLive: true})
	if got := sp.twitchHints.take(twitchHintKey("")); got != nil {
		t.Errorf("a hint with no ChannelLogin was stashed anyway: %+v", got)
	}
}

// TestWaitForTwitchLiveConsumesHint pins ENGINE-18 (report #53): a manually
// added offline channel polled GQL every 15-20 s while the monitor
// batch-polled the same channel and stashed a hint for the very same job —
// two GQL streams for one answer.
//
// Mutant: dropping the take() from the wait loop — the stash is never
// consumed and the poll still pays a GetStreamInfo round trip.
func TestWaitForTwitchLiveConsumesHint(t *testing.T) {
	c := newTwitchHintCache()
	c.stash(twitchHintKey("chan1"), &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"})

	got := takeLiveHint(c, discardLogger{}, "chan1")
	if got == nil || got.StreamID != "s1" {
		t.Fatalf("takeLiveHint = %v, want the stashed live info", got)
	}
	if again := takeLiveHint(c, discardLogger{}, "chan1"); again != nil {
		t.Error("takeLiveHint returned the same hint twice — take-once semantics are broken")
	}
	// A non-live hint is consumed, reported and NOT returned. Reported because
	// the producers only ever stash live info, so one arriving here means
	// something upstream surprised us and the wait must not erase the evidence
	// (fix round 1, Minor 1). Mutant: dropping the log line — the count below
	// is 0.
	logs := &captureLogger{}
	c.stash(twitchHintKey("chan2"), &twitch.TwitchStreamInfo{IsLive: false, StreamID: "s2"})
	if got := takeLiveHint(c, logs, "chan2"); got != nil {
		t.Error("takeLiveHint returned a non-live hint — the wait must keep polling")
	}
	if len(logs.lines()) != 1 {
		t.Errorf("a non-live hint logged %d lines, want 1 — it is consumed either way, so silence loses the only record of it", len(logs.lines()))
	}
	// A plain miss stays silent: every poll cycle takes, and almost every take
	// finds nothing.
	logs.reset()
	if got := takeLiveHint(c, logs, "never-stashed"); got != nil || len(logs.lines()) != 0 {
		t.Errorf("an ordinary miss returned %v and logged %d lines, want nil and 0", got, len(logs.lines()))
	}

	// The wait loop itself needs a live Twitch API and a 15-20 s sleep to
	// drive, so the CALL SITE is read from the syntax tree — the package's
	// technique for undrivable sites (queue_lifecycle_test.go,
	// stream_processor_early_chat_test.go). This is the arm the brief's mutant
	// (dropping the take() from the wait loop) fires: the helper still passes
	// every assertion above while the poll goes on paying its own round trip.
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "stream_processor_twitch.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse stream_processor_twitch.go: %v", err)
	}
	var wait *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "waitForTwitchLive" {
			wait = fn
		}
	}
	if wait == nil {
		t.Fatal("no waitForTwitchLive declaration in stream_processor_twitch.go")
	}
	takes := funcCallPositions(wait, "takeLiveHint")
	if len(takes) != 1 {
		t.Fatalf("waitForTwitchLive calls takeLiveHint %d time(s), want exactly 1 — the monitor's "+
			"stashed answer must be consumed by the wait, not left for a second GQL stream", len(takes))
	}
	probes := methodCallPositions(wait, "GetStreamInfo")
	if len(probes) != 1 || takes[0] > probes[0] {
		t.Errorf("takeLiveHint is at line %d and GetStreamInfo at %v — the hint must be consumed "+
			"BEFORE the poll, or it saves nothing",
			fset.Position(takes[0]).Line, probes)
	}
}

// TestWaitForTwitchLiveConsumesTheMonitorsHintForItsChannel is ENGINE-18's row
// driven end to end after fix round 1's Important 3: keyed on the JOB ID the
// consumption could never fire, because waitForTwitchLive is reached only by a
// manually added job (`tw_manual_<login>_<ns>`) while both producers stash
// under the monitor's own `tw_<streamID>` row. Keyed on the CHANNEL, the
// monitor's fetch answers the waiting job's question: it returns the stashed
// info with no GQL round trip and without sleeping out a poll interval.
//
// sp.tw is deliberately nil: the only way this test can pass is if the wait
// never reaches a poll. Mutants: keying the stash or the take on the job ID
// (the wait sleeps its 15-20 s and the assertion times out); moving the hint
// check back behind the sleep (same); dropping the take entirely (same).
func TestWaitForTwitchLiveConsumesTheMonitorsHintForItsChannel(t *testing.T) {
	_, db := testWorkerSetup(t)
	const login = "somestreamer"
	jobID := "tw_manual_" + login + "_1700000000"
	job := &database.Job{ID: jobID, VideoID: jobID, Platform: "twitch", ManuallyAdded: true,
		URL: "https://twitch.tv/" + login, Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sp := &StreamProcessor{db: db, logger: discardLogger{}, twitchHints: newTwitchHintCache()}

	// The monitor's producer, verbatim in shape: it has just fetched this
	// channel and created its OWN row, and stashes what it fetched.
	sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, StreamID: "s9", ChannelLogin: login})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		info, err := sp.waitForTwitchLive(ctx, job, login)
		done <- outcome{info, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("waitForTwitchLive: %v", got.err)
		}
		if got.info == nil || got.info.StreamID != "s9" {
			t.Fatalf("waitForTwitchLive = %+v, want the monitor's stashed info", got.info)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done // let the wait unwind on ctx rather than panic on the nil service
		t.Fatal("the wait did not consume the monitor's hint for its channel: it is sleeping out a poll interval and will then pay the GQL round trip the row exists to remove")
	}

	if hits := sp.twitchHints.Stats().Hits; hits != 1 {
		t.Errorf("hint cache hits = %d, want 1 — the wait must consume the stash, not shadow it", hits)
	}
	if again := takeLiveHint(sp.twitchHints, discardLogger{}, login); again != nil {
		t.Error("the hint survived consumption — take-once is what keeps processTwitchLive from re-reading it")
	}
}

// funcCallPositions returns the position of every call to the plain (non-method)
// function named name inside n. methodCallPositions matches a selector, and
// takeLiveHint is a package-level function.
func funcCallPositions(n ast.Node, name string) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// TestWaitForTwitchLiveReturnsWhenTheRowIsGone: a channel prune deletes
// Upcoming rows in bulk without firing OnJobDeleted, and GetJob returns
// (nil, nil) for a missing row, which the wait's cancel check dereferenced —
// a panic that processJob's recover then turned into an Error write on a row
// that no longer existed. A vanished row now ends the wait like a cancel.
//
// Mutant: drop the `currentJob == nil` arm — the goroutine panics.
func TestWaitForTwitchLiveReturnsWhenTheRowIsGone(t *testing.T) {
	_, db := testWorkerSetup(t)
	const login = "prunedstreamer"
	job := &database.Job{ID: "tw_manual_" + login + "_1700000000", Platform: "twitch",
		URL: "https://twitch.tv/" + login, Status: database.StatusUpcoming}
	// Never added: the row is already gone when the wait looks.
	sp := &StreamProcessor{db: db, logger: discardLogger{}, twitchHints: newTwitchHintCache()}

	type outcome struct {
		info  *twitch.TwitchStreamInfo
		err   error
		panic any
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{panic: r}
			}
		}()
		info, err := sp.waitForTwitchLive(context.Background(), job, login)
		done <- outcome{info: info, err: err}
	}()
	select {
	case got := <-done:
		if got.panic != nil {
			t.Fatalf("waitForTwitchLive panicked on a deleted row: %v", got.panic)
		}
		if got.info != nil || got.err != nil {
			t.Errorf("waitForTwitchLive = (%v, %v), want (nil, nil) — a cancel", got.info, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForTwitchLive kept waiting on a row that no longer exists")
	}
}

// TestWaitForTwitchLiveClearsItsProgressOnCancel: the wait writes "Waiting
// for stream..." on entry and used to clear it only when the channel went
// live, so a cancelled manual Twitch job kept reading "Waiting for
// stream..." on its Cancelled row until a Retry reset it.
//
// Mutant: drop the deferred clear — progress still holds the waiting line.
func TestWaitForTwitchLiveClearsItsProgressOnCancel(t *testing.T) {
	_, db := testWorkerSetup(t)
	const login = "cancelledstreamer"
	job := &database.Job{ID: "tw_manual_" + login + "_1700000000", VideoID: "tw_manual_" + login + "_1700000000",
		Platform: "twitch", ManuallyAdded: true, URL: "https://twitch.tv/" + login, Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sp := &StreamProcessor{db: db, logger: discardLogger{}, twitchHints: newTwitchHintCache()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sp.waitForTwitchLive(ctx, job, login)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if j, _ := db.GetJob(job.ID); j != nil && j.Progress == "Waiting for stream..." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wait never wrote its progress line")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not return on cancel")
	}
	j, err := db.GetJob(job.ID)
	if err != nil || j == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.Progress != "" {
		t.Errorf("progress after cancel = %q, want it cleared", j.Progress)
	}
}

// TestSyncTwitchJobMetadataUpdatesTheInMemoryJob: processTwitchLive and
// processTwitchVod wrote the stream's title, channel and art to the row only,
// so the job processJob passes to buildJobContext — and every embed of the
// capture — kept a manual add's "<login> — Manual Add" placeholder.
//
// Mutant: drop any one field's copy — that row of the comparison fails.
func TestSyncTwitchJobMetadataUpdatesTheInMemoryJob(t *testing.T) {
	job := &database.Job{Title: "shroud — Manual Add", ChannelName: "shroud"}
	syncTwitchJobMetadata(job, map[string]any{
		"title":              "shroud — Ranked grind",
		"channel_name":       "Shroud",
		"thumbnail_url":      "https://example.test/t.jpg",
		"channel_avatar_url": "https://example.test/a.png",
		"stream_start_time":  "2026-10-04T18:00:00Z",
		"twitch_category":    "VALORANT",
		"length_seconds":     3600,
	})
	got := []string{job.Title, job.ChannelName, job.ThumbnailURL, job.ChannelAvatarURL, job.StreamStartTime, job.TwitchCategory}
	want := []string{"shroud — Ranked grind", "Shroud", "https://example.test/t.jpg", "https://example.test/a.png", "2026-10-04T18:00:00Z", "VALORANT"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
	if job.LengthSeconds == nil || *job.LengthSeconds != 3600 {
		t.Errorf("LengthSeconds = %v, want 3600", job.LengthSeconds)
	}
}

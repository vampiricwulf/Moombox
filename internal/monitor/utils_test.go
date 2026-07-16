package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func TestIsRegexPattern(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect bool
	}{
		{"simple regex pattern", "/hello/", true},
		{"regex with flags", "/hello/i", true},
		{"regex with multiple flags", "/hello/gi", true},
		{"not a regex - no trailing slash", "/hello", false},
		{"not a regex - no slashes", "hello", false},
		{"empty string", "", false},
		{"single slash", "/", false},
		{"two slashes adjacent", "//", true},
		{"regex with complex content", "/^foo.*bar$/i", true},
		{"path-like but still valid", "/foo/bar/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRegexPattern(tt.input)
			if got != tt.expect {
				t.Errorf("isRegexPattern(%q) = %v, expected %v", tt.input, got, tt.expect)
			}
		})
	}
}

func TestParseRegexPattern(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantPattern string
		wantFlags   string
	}{
		{"simple pattern", "/hello/", "hello", ""},
		{"pattern with i flag", "/hello/i", "hello", "i"},
		{"pattern with gi flags", "/hello/gi", "hello", "gi"},
		{"pattern with anchors", "/^foo$/i", "^foo$", "i"},
		{"no trailing slash returns original", "/hello", "/hello", ""},
		{"complex pattern", "/stream.*live/i", "stream.*live", "i"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPattern, gotFlags := parseRegexPattern(tt.input)
			if gotPattern != tt.wantPattern {
				t.Errorf("parseRegexPattern(%q) pattern = %q, expected %q", tt.input, gotPattern, tt.wantPattern)
			}
			if gotFlags != tt.wantFlags {
				t.Errorf("parseRegexPattern(%q) flags = %q, expected %q", tt.input, gotFlags, tt.wantFlags)
			}
		})
	}
}

func TestNormalizeText(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"lowercase", "HELLO WORLD", "hello world"},
		{"strips accents", "caf\u00e9", "cafe"},
		{"strips umlauts", "\u00fc\u00f6\u00e4", "uoa"},
		{"already normalized", "hello", "hello"},
		{"empty string", "", ""},
		{"mixed case with diacritics", "R\u00e9sum\u00e9", "resume"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeText(tt.input)
			if got != tt.expect {
				t.Errorf("normalizeText(%q) = %q, expected %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		needle string
		expect bool
	}{
		{"substring match", "hello world", "world", true},
		{"case insensitive", "Hello World", "hello", true},
		{"no match", "hello world", "xyz", false},
		{"empty needle matches", "hello", "", true},
		{"empty text no match", "", "hello", false},
		{"diacritics ignored", "caf\u00e9 latte", "cafe", true},
		{"full match", "hello", "hello", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fuzzyMatch(tt.text, tt.needle)
			if got != tt.expect {
				t.Errorf("fuzzyMatch(%q, %q) = %v, expected %v", tt.text, tt.needle, got, tt.expect)
			}
		})
	}
}

func TestMatchTerm(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		pattern string
		expect  bool
	}{
		{"plain text regex match", "live stream today", "stream", true},
		{"plain text case insensitive", "Live Stream Today", "stream", true},
		{"regex /pattern/ syntax", "live stream today", "/stream/", true},
		{"regex /pattern/i flag", "Live STREAM Today", "/stream/i", true},
		{"regex no match", "hello world", "/^stream$/", false},
		{"regex anchored match", "stream", "/^stream$/", true},
		{"(?i) prefix pattern", "HELLO WORLD", "(?i)hello", true},
		{"invalid regex falls back to fuzzy", "hello [world", "[world", true},
		{"invalid regex in /pattern/ falls back to fuzzy", "hello [world", "/[/", false},
		{"dot-star regex", "live stream 2024", "/live.*2024/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchTerm(tt.text, tt.pattern)
			if got != tt.expect {
				t.Errorf("matchTerm(%q, %q) = %v, expected %v", tt.text, tt.pattern, got, tt.expect)
			}
		})
	}
}

func TestMetadataFailureTracker_RecordAndClear(t *testing.T) {
	tracker := NewMetadataFailureTracker()

	// First failure
	count, giveUp := tracker.RecordFailure("vid1")
	if count != 1 || giveUp {
		t.Errorf("first failure: count=%d giveUp=%v, want 1/false", count, giveUp)
	}

	// Second failure
	count, giveUp = tracker.RecordFailure("vid1")
	if count != 2 || giveUp {
		t.Errorf("second failure: count=%d giveUp=%v, want 2/false", count, giveUp)
	}

	// Third failure should trigger give up (maxMetadataFailures=3)
	count, giveUp = tracker.RecordFailure("vid1")
	if count != 3 || !giveUp {
		t.Errorf("third failure: count=%d giveUp=%v, want 3/true", count, giveUp)
	}

	// After give up, entry should be removed
	count, giveUp = tracker.RecordFailure("vid1")
	if count != 1 || giveUp {
		t.Errorf("after give up, re-record: count=%d giveUp=%v, want 1/false", count, giveUp)
	}

	// Clear removes entry
	tracker.ClearFailure("vid1")
	count, _ = tracker.RecordFailure("vid1")
	if count != 1 {
		t.Errorf("after clear: count=%d, want 1", count)
	}
}

func TestMetadataFailureTracker_Eviction(t *testing.T) {
	tracker := NewMetadataFailureTracker()

	// Fill up to maxMetadataFailuresMapSize (500)
	for i := range maxMetadataFailuresMapSize {
		tracker.RecordFailure(fmt.Sprintf("vid_%d", i))
	}

	// One more should trigger eviction
	tracker.RecordFailure("overflow_vid")

	tracker.mu.Lock()
	size := len(tracker.failures)
	tracker.mu.Unlock()

	// Should be at or below max
	if size > maxMetadataFailuresMapSize {
		t.Errorf("map size %d exceeds max %d", size, maxMetadataFailuresMapSize)
	}
}

func TestMetadataFailureTracker_ClearNonExistent(t *testing.T) {
	tracker := NewMetadataFailureTracker()
	// Should not panic
	tracker.ClearFailure("nonexistent")
}

func TestProcessYouTubeVideo_NoProbeFunc(t *testing.T) {
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID:    "test123",
		Title:      "Test Title",
		Channel:    &config.ChannelConfig{},
		ProbeVideo: nil,
		Tracker:    NewMetadataFailureTracker(),
		Logger:     &testMonitorLogger{},
	})

	if !result.ShouldProcess {
		t.Error("expected ShouldProcess=true when no probe func")
	}
	if result.Title != "Test Title" {
		t.Errorf("title: got %q, want %q", result.Title, "Test Title")
	}
}

func TestProcessYouTubeVideo_LiveStream(t *testing.T) {
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID: "live123",
		Title:   "Feed Title",
		Channel: &config.ChannelConfig{},
		ProbeVideo: func(_ context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{
				StreamStatus: "live",
				Title:        "Better Title",
				ChannelName:  "TestChannel",
			}, nil
		},
		Tracker: NewMetadataFailureTracker(),
		Logger:  &testMonitorLogger{},
	})

	if !result.ShouldProcess {
		t.Error("expected ShouldProcess=true for live stream")
	}
	if result.Title != "Better Title" {
		t.Errorf("title: got %q, want %q", result.Title, "Better Title")
	}
	if result.ChannelName != "TestChannel" {
		t.Errorf("channelName: got %q, want %q", result.ChannelName, "TestChannel")
	}
}

func TestProcessYouTubeVideo_NotAStream(t *testing.T) {
	historyAdded := false
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID: "vid123",
		Title:   "Regular Video",
		Channel: &config.ChannelConfig{},
		ProbeVideo: func(_ context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "not_a_stream"}, nil
		},
		AddToHistory: func(id string) error {
			historyAdded = true
			return nil
		},
		Tracker: NewMetadataFailureTracker(),
		Logger:  &testMonitorLogger{},
	})

	if result.ShouldProcess {
		t.Error("expected ShouldProcess=false for not_a_stream")
	}
	if !historyAdded {
		t.Error("expected video to be added to history")
	}
}

func TestProcessYouTubeVideo_NotAStreamWithIncludeNonLive(t *testing.T) {
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID: "vid123",
		Title:   "Regular Video",
		Channel: &config.ChannelConfig{IncludeNonLiveContent: true},
		ProbeVideo: func(_ context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "not_a_stream"}, nil
		},
		Tracker: NewMetadataFailureTracker(),
		Logger:  &testMonitorLogger{},
	})

	if !result.ShouldProcess {
		t.Error("expected ShouldProcess=true when include_non_live_content=true")
	}
}

func TestProcessYouTubeVideo_ProbeError(t *testing.T) {
	tracker := NewMetadataFailureTracker()

	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID: "err123",
		Title:   "Test",
		Channel: &config.ChannelConfig{},
		ProbeVideo: func(_ context.Context, videoID string) (*VideoProbeResult, error) {
			return nil, fmt.Errorf("network error")
		},
		Tracker: tracker,
		Logger:  &testMonitorLogger{},
	})

	if result.ShouldProcess {
		t.Error("expected ShouldProcess=false on probe error")
	}
}

func TestProcessYouTubeVideo_UnknownTitleNotOverwrite(t *testing.T) {
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID: "vid123",
		Title:   "Good Feed Title",
		Channel: &config.ChannelConfig{},
		ProbeVideo: func(_ context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{
				StreamStatus: "live",
				Title:        "Unknown Title", // Should not overwrite
			}, nil
		},
		Tracker: NewMetadataFailureTracker(),
		Logger:  &testMonitorLogger{},
	})

	if result.Title != "Good Feed Title" {
		t.Errorf("title should not be overwritten by 'Unknown Title': got %q", result.Title)
	}
}

func TestIsDenied_CanonicalTable(t *testing.T) {
	// Spec §9 table — all seven rows (plus one extra not_a_stream row). Both
	// conjuncts load-bearing.
	cases := []struct {
		status, playability string
		want                bool
	}{
		{"upcoming", "members_only", true},
		{"upcoming", "login_required", true},
		{"upcoming", "ok", false},             // genuine upcoming — goal 3
		{"upcoming", "age_restricted", false}, // premieres reach upcoming with non-ok playability
		{"upcoming", "unknown", false},        // unknown is not a refusal
		{"vod", "age_restricted", false},      // formats came back — grounded
		{"vod", "members_only", false},        // same — the status conjunct allows this
		{"not_a_stream", "login_required", false},
	}
	for _, c := range cases {
		if got := isDenied(c.status, c.playability); got != c.want {
			t.Errorf("isDenied(%q,%q) = %v, want %v", c.status, c.playability, got, c.want)
		}
	}
}

func TestProbeAndClassify_Outcomes(t *testing.T) {
	ch := &config.ChannelConfig{Name: "c"}
	mk := func(probe func(context.Context, string) (*VideoProbeResult, error)) ProbeClassifyParams {
		return ProbeClassifyParams{Ctx: context.Background(), VideoID: "v", Channel: ch,
			ProbeVideo: probe, Tracker: NewMetadataFailureTracker(), Logger: silentLogger{}}
		// Cooldown nil => no suppression (ProbeCooldown methods tolerate a nil receiver).
	}
	// probed
	r := probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "T", PublishedAt: "2026-07-14T20:00:00Z", PublishedPrecision: "started", PlayabilityError: "ok"}, nil
	}))
	if r.Outcome != OutcomeProbed || r.StreamStatus != "vod" || r.PublishedPrecision != "started" {
		t.Fatalf("probed: %+v", r)
	}
	// denied — upcoming + members_only
	r = probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "members_only"}, nil
	}))
	if r.Outcome != OutcomeDenied || r.PlayabilityError != "members_only" {
		t.Fatalf("denied: %+v (PlayabilityError must be carried — the escalation reads it)", r)
	}
	// errored
	r = probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return nil, errors.New("boom")
	}))
	if r.Outcome != OutcomeErrored {
		t.Fatalf("errored: %+v", r)
	}
}

func TestProbeAndClassify_NoHistoryWrites(t *testing.T) {
	// The split exists because ProcessYouTubeVideo has AddToHistory side effects.
	// probeAndClassify has NO AddToHistory parameter, which the compiler
	// enforces — this test pins the DECAPI side instead: ProcessYouTubeVideo
	// (composed) still writes history on a skipped vod.
	var histCalls int
	res := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		Ctx: context.Background(), VideoID: "v", Channel: &config.ChannelConfig{Name: "c"},
		ProbeVideo: func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "vod", Title: "T", PlayabilityError: "ok"}, nil
		},
		AddToHistory: func(id string) error { histCalls++; return nil },
		Tracker:      NewMetadataFailureTracker(), Logger: silentLogger{},
	})
	if res.ShouldProcess {
		t.Fatal("skipped vod (IncludeNonLiveContent false) must not process")
	}
	if histCalls != 1 {
		t.Fatalf("DECAPI path must keep its history write, got %d calls", histCalls)
	}
}

type testMonitorLogger struct{}

func (l *testMonitorLogger) Debug(msg string, args ...any) {}
func (l *testMonitorLogger) Info(msg string, args ...any)  {}
func (l *testMonitorLogger) Warn(msg string, args ...any)  {}
func (l *testMonitorLogger) Error(msg string, args ...any) {}

func TestMatchesTerms(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		channel *config.ChannelConfig
		expect  bool
	}{
		{
			name: "no terms configured matches everything",
			text: "any text at all",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{},
			},
			expect: true,
		},
		{
			name: "simple term matches",
			text: "live stream gaming",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{Simple: "gaming"},
			},
			expect: true,
		},
		{
			name: "simple term does not match",
			text: "live stream cooking",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{Simple: "gaming"},
			},
			expect: false,
		},
		{
			name: "named terms one matches",
			text: "live gaming stream",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{
					IsMap: true,
					Named: map[string]string{
						"title": "gaming",
						"other": "music",
					},
				},
			},
			expect: true,
		},
		{
			name: "named terms none match",
			text: "live cooking stream",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{
					IsMap: true,
					Named: map[string]string{
						"title": "gaming",
						"other": "music",
					},
				},
			},
			expect: false,
		},
		{
			name: "empty named terms matches everything",
			text: "anything",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{
					IsMap: true,
					Named: map[string]string{},
				},
			},
			expect: true,
		},
		{
			name: "regex term in simple",
			text: "stream started at 10pm",
			channel: &config.ChannelConfig{
				Terms: config.ChannelTerms{Simple: "/stream.*\\d+pm/"},
			},
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesTerms(tt.text, tt.channel)
			if got != tt.expect {
				t.Errorf("MatchesTerms(%q, ...) = %v, expected %v", tt.text, got, tt.expect)
			}
		})
	}
}

func TestProbeCooldownAllowsFreshAndBlocksRecent(t *testing.T) {
	// ShouldProbe returns true for unseen IDs and false within the cooldown
	// window after Record — protects feed/DECAPI cycles from re-probing
	// every ~10 minutes (audit reports/monitor.md #5).
	cd := NewProbeCooldown(50 * time.Millisecond)

	if !cd.ShouldProbe("vid-1") {
		t.Fatal("ShouldProbe on unseen videoID: want true, got false")
	}

	cd.Record("vid-1")
	if cd.ShouldProbe("vid-1") {
		t.Errorf("ShouldProbe immediately after Record: want false, got true")
	}

	// Different ID is independent
	if !cd.ShouldProbe("vid-2") {
		t.Errorf("ShouldProbe on different unseen videoID: want true, got false")
	}

	// After the cooldown elapses the slot is reusable. 80ms gives the test
	// a generous margin without slowing the suite noticeably.
	time.Sleep(80 * time.Millisecond)
	if !cd.ShouldProbe("vid-1") {
		t.Errorf("ShouldProbe after cooldown elapses: want true, got false")
	}
}

// TestMonitorsHaveIndependentProbeCooldowns pins the per-monitor cooldown
// contract: the feed (RSS) and DECAPI monitors must NOT share a probe
// cooldown. Sharing let a slow RSS probe of an upcoming video record the full
// window and block DECAPI — the fast ~15s live-detector — from re-probing the
// same video, delaying live-transition detection by up to the cooldown window.
func TestMonitorsHaveIndependentProbeCooldowns(t *testing.T) {
	log := silentLogger{}
	// Constructors only stash store/db; nil is fine for this construction-only
	// check and keeps the test free of a Store/DB fixture.
	feedMon := NewFeedMonitor(nil, nil, log)
	decapiMon := NewDecapiMonitor(nil, nil, log)

	if feedMon.ProbeCooldown == nil || decapiMon.ProbeCooldown == nil {
		t.Fatal("each monitor must own a probe cooldown")
	}
	if feedMon.ProbeCooldown == decapiMon.ProbeCooldown {
		t.Fatal("feed and DECAPI must NOT share a probe cooldown instance")
	}

	// Enable a window on both (the default is 0 = disabled, under which
	// ShouldProbe is always true and the independence isn't observable).
	feedMon.ProbeCooldown.SetDuration(time.Hour)
	decapiMon.ProbeCooldown.SetDuration(time.Hour)

	// Behavioral proof: a probe recorded by feed must not block DECAPI.
	const videoID = "dQw4w9WgXcQ"
	feedMon.ProbeCooldown.Record(videoID)
	if feedMon.ProbeCooldown.ShouldProbe(videoID) {
		t.Error("feed cooldown should block feed's own re-probe right after Record")
	}
	if !decapiMon.ProbeCooldown.ShouldProbe(videoID) {
		t.Error("DECAPI must stay free to probe a video feed just probed — cooldowns are independent")
	}
}

func TestProbeCooldownDisabledAlwaysProbes(t *testing.T) {
	// A window of 0 disables the cooldown: ShouldProbe is always true and
	// Record is a no-op (the map stays empty), so every cycle re-probes.
	// This is the default behavior — the operator opts into rate-limiting.
	cd := NewProbeCooldown(0)

	cd.Record("vid-1")
	if !cd.ShouldProbe("vid-1") {
		t.Fatal("disabled cooldown must always allow probing")
	}
	cd.mu.Lock()
	size := len(cd.lastProbe)
	cd.mu.Unlock()
	if size != 0 {
		t.Errorf("disabled cooldown Record must be a no-op: want empty map, got %d entries", size)
	}
}

func TestProbeCooldownSetDurationHotReload(t *testing.T) {
	// SetDuration lets a monitor hot-reload the configured window each cycle.
	// Enabling a window then applies to subsequently recorded probes; setting
	// it back to 0 re-opens probing immediately.
	cd := NewProbeCooldown(0)

	cd.SetDuration(time.Hour)
	cd.Record("vid-1")
	if cd.ShouldProbe("vid-1") {
		t.Fatal("after enabling a window, a just-recorded probe must be blocked")
	}

	cd.SetDuration(0)
	if !cd.ShouldProbe("vid-1") {
		t.Fatal("after disabling the window, probing must re-open immediately")
	}
}

func TestProbeCooldownNilReceiver(t *testing.T) {
	// Both methods accept a nil receiver so call sites can pass an unset
	// optional pointer without nil-checks.
	var cd *ProbeCooldown

	if !cd.ShouldProbe("vid-x") {
		t.Errorf("nil ShouldProbe: want true, got false")
	}
	cd.Record("vid-x") // must not panic
}

func TestProbeCooldownEvictsExcess(t *testing.T) {
	// The map is capped at probeCooldownMaxSize. Adding more entries
	// evicts the oldest so the cache can't grow unbounded over a long
	// uptime with many distinct video IDs.
	cd := NewProbeCooldown(time.Hour)

	for i := range probeCooldownMaxSize + 50 {
		cd.Record(fmt.Sprintf("vid-%d", i))
	}

	cd.mu.Lock()
	size := len(cd.lastProbe)
	cd.mu.Unlock()

	if size > probeCooldownMaxSize {
		t.Errorf("after %d Records: want size <= %d, got %d",
			probeCooldownMaxSize+50, probeCooldownMaxSize, size)
	}
}

func TestProbeCooldownConcurrentSafe(t *testing.T) {
	// Race-detector smoke test for the shared map. Run with `go test -race`
	// to confirm no concurrent map access.
	cd := NewProbeCooldown(time.Hour)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range 100 {
				id := fmt.Sprintf("vid-%d-%d", workerID, i)
				cd.ShouldProbe(id)
				cd.Record(id)
			}
		}(w)
	}
	wg.Wait()
}

func TestProcessYouTubeVideoSkipsWithinCooldown(t *testing.T) {
	// When the cooldown is active for a video, ProcessYouTubeVideo must
	// short-circuit before invoking ProbeVideo. Otherwise the cooldown
	// would only spread out the work, not eliminate redundant probes.
	cd := NewProbeCooldown(time.Hour)
	cd.Record("vid-1")

	probeCalls := 0
	res := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		Ctx:     context.Background(),
		VideoID: "vid-1",
		Title:   "test",
		Channel: &config.ChannelConfig{ID: "ch-1", Name: "Test"},
		ProbeVideo: func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			probeCalls++
			return &VideoProbeResult{StreamStatus: "live"}, nil
		},
		Tracker:  NewMetadataFailureTracker(),
		Cooldown: cd,
		Logger:   silentLogger{},
	})

	if probeCalls != 0 {
		t.Errorf("expected 0 probe calls within cooldown, got %d", probeCalls)
	}
	if res.ShouldProcess {
		t.Errorf("ShouldProcess: want false during cooldown, got true")
	}
}

// silentLogger discards log output for tests that don't need to inspect it.
type silentLogger struct{}

func (silentLogger) Debug(msg string, args ...any) {}
func (silentLogger) Info(msg string, args ...any)  {}
func (silentLogger) Warn(msg string, args ...any)  {}
func (silentLogger) Error(msg string, args ...any) {}

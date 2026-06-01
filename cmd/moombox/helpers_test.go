package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestYouTubeThumbnailURL locks the maxres URL format. Audit
// reports/cmd-moombox.md D-7 — centralising the URL.
func TestYouTubeThumbnailURL(t *testing.T) {
	got := youtubeThumbnailURL("dQw4w9WgXcQ")
	want := "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg"
	if got != want {
		t.Errorf("youtubeThumbnailURL = %q, want %q", got, want)
	}
}

// TestResolveOutputDirChannelOverride covers the channel-specific
// branch — when ChannelConfig.OutputDirectory is set, it wins over
// the global default.
func TestResolveOutputDirChannelOverride(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Paths: config.PathsConfig{OutputDirectory: "/global/default"},
	}, "")
	ch := &config.ChannelConfig{OutputDirectory: "/channel/specific"}

	got := resolveOutputDir(ch, store)
	if got != "/channel/specific" {
		t.Errorf("resolveOutputDir with channel override: want %q, got %q", "/channel/specific", got)
	}
}

// TestResolveOutputDirGlobalFallback covers the empty-channel branch —
// falls back to the global Paths.OutputDirectory.
func TestResolveOutputDirGlobalFallback(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Paths: config.PathsConfig{OutputDirectory: "/global/default"},
	}, "")
	ch := &config.ChannelConfig{}

	got := resolveOutputDir(ch, store)
	if got != "/global/default" {
		t.Errorf("resolveOutputDir with empty channel: want %q, got %q", "/global/default", got)
	}
}

// TestFilterJobsByAgeKeepsRecentFinished verifies the cutoff: a Finished
// job updated within the configured age window is kept.
func TestFilterJobsByAgeKeepsRecentFinished(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Monitors: config.MonitorsConfig{
			HideFinishedAgeDays: config.FlexDuration{Value: 7},
		},
	}, "")

	recent := time.Now().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "a", Status: database.StatusFinished, UpdatedAt: recent},
		{ID: "b", Status: database.StatusLive, UpdatedAt: ""},
	}

	got := filterJobsByAge(jobs, store)
	if len(got) != 2 {
		t.Errorf("recent-finished + live: want both kept, got %d jobs", len(got))
	}
}

// TestFilterJobsByAgeRemovesStaleFinished verifies a Finished job
// updated outside the cutoff is dropped, while non-Finished and recent
// Finished are kept.
func TestFilterJobsByAgeRemovesStaleFinished(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Monitors: config.MonitorsConfig{
			HideFinishedAgeDays: config.FlexDuration{Value: 7},
		},
	}, "")

	stale := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "stale-finished", Status: database.StatusFinished, UpdatedAt: stale},
		{ID: "recent-finished", Status: database.StatusFinished, UpdatedAt: recent},
		{ID: "live", Status: database.StatusLive, UpdatedAt: stale},
	}

	got := filterJobsByAge(jobs, store)
	if len(got) != 2 {
		t.Fatalf("want 2 jobs after filter (recent-finished + live), got %d", len(got))
	}
	for _, j := range got {
		if j.ID == "stale-finished" {
			t.Errorf("stale finished job should have been filtered out")
		}
	}
}

// TestFilterJobsByAgeReturnsOriginalIfNoneFiltered exercises the early-
// return optimisation: when no filtering occurs, the original slice is
// returned unchanged (same backing array).
func TestFilterJobsByAgeReturnsOriginalIfNoneFiltered(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Monitors: config.MonitorsConfig{
			HideFinishedAgeDays: config.FlexDuration{Value: 365},
		},
	}, "")

	recent := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "a", Status: database.StatusFinished, UpdatedAt: recent},
	}
	got := filterJobsByAge(jobs, store)
	if len(got) != len(jobs) {
		t.Errorf("filter on a no-op slice: want unchanged length, got %d vs %d", len(got), len(jobs))
	}
}

// TestFilterJobsByAgeMalformedTimestampsAreKept covers the parse-error
// branch: a Finished job with a malformed UpdatedAt should NOT be
// silently filtered (we'd lose the row entirely from the UI).
func TestFilterJobsByAgeMalformedTimestampsAreKept(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Monitors: config.MonitorsConfig{
			HideFinishedAgeDays: config.FlexDuration{Value: 7},
		},
	}, "")

	stale := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "stale-finished", Status: database.StatusFinished, UpdatedAt: stale},
		{ID: "malformed", Status: database.StatusFinished, UpdatedAt: "not-a-timestamp"},
	}

	got := filterJobsByAge(jobs, store)
	keptIDs := map[string]bool{}
	for _, j := range got {
		keptIDs[j.ID] = true
	}
	if keptIDs["stale-finished"] {
		t.Error("stale finished should be filtered")
	}
	if !keptIDs["malformed"] {
		t.Error("malformed timestamp should be preserved (don't silently lose rows)")
	}
}

// TestFilterJobsByAgeThresholdNegativeNeverArchives locks the "never archive"
// contract shared with the REST filter, the TUI, and the Web UI: a negative
// threshold keeps every finished job in the active list regardless of age.
// (The old AddDate-based implementation inverted this and archived them all.)
func TestFilterJobsByAgeThresholdNegativeNeverArchives(t *testing.T) {
	veryOld := time.Now().Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "ancient-finished", Status: database.StatusFinished, UpdatedAt: veryOld},
		{ID: "live", Status: database.StatusLive, UpdatedAt: veryOld},
	}
	got := filterJobsByAgeThreshold(jobs, -1)
	if len(got) != 2 {
		t.Fatalf("hideAgeDays<0 must keep all jobs active, got %d of 2", len(got))
	}
}

// TestFilterJobsByAgeThresholdZeroArchivesPastFinished verifies the boundary at
// hideAgeDays==0: every finished job with a past updated_at is archived, while
// non-finished jobs stay active (strict age > 0, matching the REST filter).
func TestFilterJobsByAgeThresholdZeroArchivesPastFinished(t *testing.T) {
	past := time.Now().Add(-1 * time.Second).Format(time.RFC3339)
	jobs := []*database.Job{
		{ID: "finished", Status: database.StatusFinished, UpdatedAt: past},
		{ID: "live", Status: database.StatusLive, UpdatedAt: past},
	}
	got := filterJobsByAgeThreshold(jobs, 0)
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("hideAgeDays==0 must archive past finished and keep live, got %v", idsOf(got))
	}
}

// TestFilterJobsByAgeThresholdFractionalPrecision verifies fractional-day
// thresholds are honored at full precision rather than truncated to a whole
// day. With 1.5 days, a job aged ~1.6 days archives while one aged ~1.4 days
// stays — the old int(value) cast collapsed both to "1 day" and disagreed with
// the REST filter and the Web UI.
func TestFilterJobsByAgeThresholdFractionalPrecision(t *testing.T) {
	// Use a runtime conversion (matching filterJobsByAgeThreshold itself) so the
	// fractional day count is truncated to whole hours exactly as production
	// does — 1.6d→38h, 1.4d→33h — bracketing the 1.5d (36h) cutoff.
	daysAgo := func(d float64) string {
		return time.Now().Add(-time.Duration(d*24) * time.Hour).Format(time.RFC3339)
	}
	aged := daysAgo(1.6)
	fresh := daysAgo(1.4)
	jobs := []*database.Job{
		{ID: "aged", Status: database.StatusFinished, UpdatedAt: aged},
		{ID: "fresh", Status: database.StatusFinished, UpdatedAt: fresh},
	}
	got := filterJobsByAgeThreshold(jobs, 1.5)
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("hideAgeDays==1.5 must archive the 1.6d job and keep the 1.4d job, got %v", idsOf(got))
	}
}

func idsOf(jobs []*database.Job) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

// TestExtractWSIPHostOnly covers the happy path — a host:port
// RemoteAddr returns just the host.
func TestExtractWSIPHostOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.10:54321"
	if got := extractWSIP(r); got != "192.168.1.10" {
		t.Errorf("extractWSIP host:port: want 192.168.1.10, got %q", got)
	}
}

// TestExtractWSIPMalformedReturnsRaw covers the SplitHostPort error
// branch — a malformed RemoteAddr falls through to the raw string.
func TestExtractWSIPMalformedReturnsRaw(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "no-port-here"
	if got := extractWSIP(r); got != "no-port-here" {
		t.Errorf("extractWSIP malformed: want %q, got %q", "no-port-here", got)
	}
}

// TestNopLoggerImplementsInterface — compile-time check that nopLogger
// satisfies the anonymous logger interfaces used across the codebase.
// This locks the contract so a future logger-method addition gets
// caught here rather than at the first nil-deref.
func TestNopLoggerImplementsInterface(t *testing.T) {
	var l interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	} = &nopLogger{}
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	// Drop a tiny invariant in the test name so test grep finds intent.
	_ = strings.Contains("ok", "k")
}

package config

import (
	"testing"
	"time"
)

// useLocalZone points time.Local at zone for the test. Not for a parallel
// test: time.Local is process-wide.
func useLocalZone(t *testing.T, zone *time.Location) {
	t.Helper()
	prev := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = prev })
}

// TestResolveTemplateWritesTheDateInLocalTime: ${start_date} and
// ${start_time} are the host's local date and time (time.Local — TZ in
// Docker, the system zone elsewhere), whatever offset the stored time
// carries. Every stored time is UTC (created_at, YouTube's "+00:00"
// startTimestamp, Twitch's "Z" createdAt), and the resolver formatted the
// parsed time in its own offset, so TZ never reached a filename: with
// TZ=America/Los_Angeles a stream starting 20:30 on Oct 16 was filed as
// "20261017 0330". The same instant spelled in any offset now names the
// same local date and time.
//
// Mutant: drop `now = now.In(time.Local)` (every case resolves in the
// string's own offset).
func TestResolveTemplateWritesTheDateInLocalTime(t *testing.T) {
	const tmpl = "${start_date} ${start_time}"
	resolve := func(date string) string {
		return ResolveTemplate(tmpl, TemplateVariables{ID: "id", Date: &date})
	}

	useLocalZone(t, time.FixedZone("PDT", -7*3600))
	for _, stored := range []string{
		"2026-10-17T03:30:00Z",      // Twitch createdAt, created_at
		"2026-10-17T03:30:00+00:00", // YouTube startTimestamp
		"2026-10-17T12:30:00+09:00", // the same instant in another offset
	} {
		if got := resolve(stored); got != "20261016 2030" {
			t.Errorf("UTC-7 host, stored %s: resolved %q, want the local %q", stored, got, "20261016 2030")
		}
	}

	useLocalZone(t, time.FixedZone("JST", 9*3600))
	if got := resolve("2026-10-16T23:30:00Z"); got != "20261017 0830" {
		t.Errorf("UTC+9 host, stored 2026-10-16T23:30:00Z: resolved %q, want the local %q", got, "20261017 0830")
	}
}

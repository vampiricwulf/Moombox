package routes

import (
	"testing"
	"time"
)

// TestStatusCarriesTheHostsUTCOffset: /api/status says how far the host's
// zone (time.Local, the one config.ResolveTemplate writes
// ${start_date}/${start_time} in) is from UTC, east positive, so the
// dashboard's template example shows the date a recording made today gets
// rather than the browser's, which is not the host's for a remote viewer
// or for a container whose TZ differs from the viewer's.
//
// Mutants: drop the key (missing); report the offset in seconds or with
// its sign flipped (the value is wrong).
//
// Do not add t.Parallel(): it sets the process-wide time.Local.
func TestStatusCarriesTheHostsUTCOffset(t *testing.T) {
	prev := time.Local
	t.Cleanup(func() { time.Local = prev })
	for _, tc := range []struct {
		zone *time.Location
		want float64
	}{
		{time.FixedZone("PDT", -7*3600), -420},
		{time.FixedZone("IST", 5*3600+1800), 330},
		{time.UTC, 0},
	} {
		time.Local = tc.zone
		got, ok := getSidecarStatusBody(t)["utcOffsetMinutes"]
		if !ok {
			t.Fatal("utcOffsetMinutes missing from /api/status")
		}
		if got != tc.want {
			t.Errorf("host zone %s: utcOffsetMinutes = %v, want %v", tc.zone, got, tc.want)
		}
	}
}

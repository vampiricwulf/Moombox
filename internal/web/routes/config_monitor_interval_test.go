package routes

import "testing"

// A monitor reads its check interval only when it arms the next cycle, so a
// dashboard save that changed one left the armed timer on the old delay — up
// to a day for the feed — while the same change saved from the TUI, which
// kicks the monitors, applied at once. The PUT now calls
// OnMonitorIntervalChange when an interval changed, and leaves a save that
// also carried channels to OnChannelChange's kick.
//
// Mutants: drop the callback — the three "changed" rows fire 0 times; fire it
// regardless of the change — the "unchanged" row fires; fire it beside
// OnChannelChange — the channels row fires twice.
func TestConfigPutKicksTheMonitorsWhenAnIntervalChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    int
	}{
		{"feed", map[string]any{"monitors": map[string]any{"feed_check_interval": float64(5)}}, 1},
		{"decapi", map[string]any{"monitors": map[string]any{"decapi_check_interval": float64(120)}}, 1},
		{"twitch", map[string]any{"monitors": map[string]any{"twitch_check_interval": float64(30)}}, 1},
		{"unchanged", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, 0},
		{"with channels", map[string]any{
			"monitors": map[string]any{"feed_check_interval": float64(5)},
			"channels": []any{},
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kicks := 0
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnChannelChange:         func() { kicks++ },
				OnMonitorIntervalChange: func() { kicks++ },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if kicks != tc.want {
				t.Errorf("the monitors were kicked %d times, want %d", kicks, tc.want)
			}
		})
	}
}

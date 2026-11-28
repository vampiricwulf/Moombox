package routes

import "testing"

// monitors.latency_backfill is hot-reloadable (owner ruling RV-20): the PUT
// writes it and reports a change, so the walk is woken to stop or resume at
// once, and a save that did not move it reports nothing.
//
// Mutants: drop the latency_backfill arm from applyConfigUpdates (the
// changed row reports nothing: the value never moved); fire the callback on
// every save (the unchanged rows report).
func TestConfigPutReportsALatencyBackfillChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    []bool
	}{
		{"turned off", map[string]any{"monitors": map[string]any{"latency_backfill": false}}, []bool{false}},
		{"saved on while on", map[string]any{"monitors": map[string]any{"latency_backfill": true}}, nil},
		{"untouched", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []bool
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnLatencyBackfillChange: func(on bool) { got = append(got, on) },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("OnLatencyBackfillChange got %v, want %v", got, tc.want)
			}
		})
	}
}

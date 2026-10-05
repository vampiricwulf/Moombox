package routes

import "testing"

// The disk gauge and alerts re-read their thresholds and the output directory
// only on the periodic check, every third two-minute tick, so a save of any of
// them showed the old reading for up to six minutes. The PUT now calls
// OnDiskSettingsChange when one of the three changes.
//
// Mutants: drop the callback — the three "changed" rows fire 0 times; fire it
// on every save — the "unchanged" row fires.
func TestConfigPutReportsADiskSettingsChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    int
	}{
		{"warn", map[string]any{"disk": map[string]any{"disk_warn_percent": float64(80)}}, 1},
		{"critical", map[string]any{"disk": map[string]any{"disk_critical_percent": float64(98)}}, 1},
		{"output dir", map[string]any{"paths": map[string]any{"output_directory": t.TempDir()}}, 1},
		{"unchanged", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fires := 0
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnDiskSettingsChange: func() { fires++ },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if fires != tc.want {
				t.Errorf("OnDiskSettingsChange fired %d times, want %d", fires, tc.want)
			}
		})
	}
}

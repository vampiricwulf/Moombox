package routes

import "testing"

// The TUI's status bar re-read its platform indicators only on an auth
// transition, so a dashboard save that switched one off left it up there.
// The PUT now calls OnActivePlatformsChange when the active set changes.
//
// Mutants: drop the callback — the "changed" row fires 0 times; fire it on
// every save — the "unchanged" row fires.
func TestConfigPutReportsAnActivePlatformsChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    int
	}{
		{"changed", map[string]any{"cookies": map[string]any{"active_platforms": []any{"twitch"}}}, 1},
		{"unchanged", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fires := 0
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnActivePlatformsChange: func() { fires++ },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if fires != tc.want {
				t.Errorf("OnActivePlatformsChange fired %d times, want %d", fires, tc.want)
			}
		})
	}
}

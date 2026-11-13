package routes

import "testing"

// downloader.keep_awake is hot-reloadable (owner ruling R2-X5): the PUT writes
// it and reports a change, so a hold in place is released at once.
//
// Mutants: drop the keep_awake arm from applyConfigUpdates (the changed row
// reports nothing: the value never moved); fire the callback on every save
// (the unchanged row reports).
func TestConfigPutReportsAKeepAwakeChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    []bool
	}{
		{"turned off", map[string]any{"downloader": map[string]any{"keep_awake": false}}, []bool{false}},
		{"untouched", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []bool
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnKeepAwakeChange: func(on bool) { got = append(got, on) },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("OnKeepAwakeChange got %v, want %v", got, tc.want)
			}
		})
	}
}

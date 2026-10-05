package routes

import "testing"

// A segment_workers above the warning threshold was logged only at boot; the
// PUT now reports a change so the caller can warn at save time too.
//
// Mutants: drop the callback — the changed row reports nothing; fire it on
// every save — the unchanged row reports.
func TestConfigPutReportsASegmentWorkersChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		want    []int
	}{
		{"changed", map[string]any{"downloader": map[string]any{"segment_workers": float64(32)}}, []int{32}},
		{"unchanged", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnSegmentWorkersChange: func(n int) { got = append(got, n) },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d (%s), want 200", rec.Code, rec.Body)
			}
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("OnSegmentWorkersChange got %v, want %v", got, tc.want)
			}
		})
	}
}

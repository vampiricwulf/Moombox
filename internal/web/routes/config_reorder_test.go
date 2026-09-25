package routes

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestReorderValidatorsRejectNegativesAndAcceptZero mirrors config.Validate:
// 0 is the documented "unbounded" value on both keys, so the PUT must accept
// it and reject only a negative.
//
// MUTANT: write either guard as `v < 1` (the shape max_video_resolution used)
// — the two zero rows report a field error and the dashboard can no longer
// save "unbounded" at all.
func TestReorderValidatorsRejectNegativesAndAcceptZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		field   string
		wantErr bool
	}{
		{"per-job -1", map[string]any{"downloader": map[string]any{"reorder_buffer_mb": float64(-1)}}, "downloader.reorder_buffer_mb", true},
		{"budget -1", map[string]any{"downloader": map[string]any{"reorder_budget_mb": float64(-1)}}, "downloader.reorder_budget_mb", true},
		{"per-job 0", map[string]any{"downloader": map[string]any{"reorder_buffer_mb": float64(0)}}, "downloader.reorder_buffer_mb", false},
		{"budget 0", map[string]any{"downloader": map[string]any{"reorder_budget_mb": float64(0)}}, "downloader.reorder_budget_mb", false},
		{"per-job 8192", map[string]any{"downloader": map[string]any{"reorder_buffer_mb": float64(8192)}}, "downloader.reorder_buffer_mb", false},
	} {
		errs := validateConfigUpdates(tc.updates)
		_, got := errs[tc.field]
		if got != tc.wantErr {
			t.Errorf("%s: field error on %s = %v, want %v (errs=%v)", tc.name, tc.field, got, tc.wantErr, errs)
		}
	}
}

// TestApplyConfigUpdatesWritesBothReorderKeys — without this the PUT would
// validate the keys and then drop them on the floor, which reads to the
// operator as a save that silently reverted.
//
// MUTANT: drop either assignment from applyConfigUpdates — that field keeps
// its old value and the assertion names it.
func TestApplyConfigUpdatesWritesBothReorderKeys(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.ReorderBufferMB = 1
	cfg.Downloader.ReorderBudgetMB = 2

	applyConfigUpdates(cfg, map[string]any{"downloader": map[string]any{
		"reorder_buffer_mb": float64(512),
		"reorder_budget_mb": float64(0),
	}})

	if cfg.Downloader.ReorderBufferMB != 512 {
		t.Errorf("ReorderBufferMB = %d, want 512", cfg.Downloader.ReorderBufferMB)
	}
	if cfg.Downloader.ReorderBudgetMB != 0 {
		t.Errorf("ReorderBudgetMB = %d, want 0 (an explicit unbounded must not be dropped as a zero value)",
			cfg.Downloader.ReorderBudgetMB)
	}
}

// TestConfigPutFiresTheReorderCallbackOnlyOnAChange — the engine's
// process-wide budget is set once at boot and re-applied from this callback;
// without the change gate every unrelated save reconfigures it (harmless but
// noisy), and without the callback an operator who raises the budget mid-
// download sees nothing happen until a restart.
//
// MUTANT: fire unconditionally — the third sub-case's count is 1, not 0.
// MUTANT: gate on reorder_buffer_mb only — the second sub-case never fires.
func TestConfigPutFiresTheReorderCallbackOnlyOnAChange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		updates   map[string]any
		wantFires int
	}{
		{"per-job changed", map[string]any{"downloader": map[string]any{"reorder_buffer_mb": float64(512)}}, 1},
		{"budget changed", map[string]any{"downloader": map[string]any{"reorder_budget_mb": float64(512)}}, 1},
		{"neither changed", map[string]any{"logs": map[string]any{"log_level": "DEBUG"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fires := 0
			var seen config.DownloaderConfig
			_, rec := putConfigWithReorderHook(t, tc.updates, &ConfigRoutesCallbacks{
				OnReorderBudgetChange: func(d config.DownloaderConfig) { fires++; seen = d },
			})
			if rec.Code != 200 {
				t.Fatalf("PUT /api/config = %d, want 200", rec.Code)
			}
			if fires != tc.wantFires {
				t.Fatalf("OnReorderBudgetChange fired %d times, want %d", fires, tc.wantFires)
			}
			if tc.wantFires == 1 && seen.ReorderBufferMB == 0 && seen.ReorderBudgetMB == 0 {
				t.Error("the callback received an empty DownloaderConfig — it must carry the SAVED section")
			}
		})
	}
}

// putConfigWithReorderHook drives PUT /api/config against a Defaults() store
// with caller-supplied callbacks, returning the store and the recorder.
// Distinct from this package's existing putConfig, whose fixture fixes the
// callback set and asserts a 200 for the caller.
func putConfigWithReorderHook(t *testing.T, updates map[string]any, cb *ConfigRoutesCallbacks) (*config.Store, *httptest.ResponseRecorder) {
	t.Helper()
	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))
	r := chi.NewRouter()
	ConfigRoutes(r, store, cb)

	body, err := json.Marshal(updates)
	if err != nil {
		t.Fatalf("marshal updates: %v", err)
	}
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return store, rec
}

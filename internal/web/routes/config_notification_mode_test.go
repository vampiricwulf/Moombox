package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestNotificationModeValidator mirrors config.Validate: the PUT must refuse
// exactly what the file loader refuses, or the dashboard could save a value
// the next boot normalises away behind the operator's back.
func TestNotificationModeValidator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    any
		wantErr bool
	}{
		{"edit", "edit", false},
		{"separate", "separate", false},
		{"empty", "", false},
		{"typo", "eidt", true},
		{"capitalised", "Edit", true},
		// N2b's decode gate reports a type mismatch as notifications[i].<key>
		// before this validator ever runs — the same 400 `enabled: "false"`
		// gets (config_notifications_test.go:211).
		{"not a string", float64(1), true},
	} {
		updates := map[string]any{"notifications": []any{
			map[string]any{"url": "discord://1/a", "mode": tc.mode},
		}}
		errs := validateConfigUpdates(updates)
		_, got := errs["notifications[0].mode"]
		if got != tc.wantErr {
			t.Errorf("%s: field error = %v, want %v (errs=%v)", tc.name, got, tc.wantErr, errs)
		}
	}
}

// TestApplyConfigUpdatesWritesMode — without this the PUT would validate the
// key and then drop it, which reads to the operator as a save that reverted.
// It passes with no applyConfigUpdates edit — decodeConfigEntries carries the
// key — which is exactly what it exists to keep true.
func TestApplyConfigUpdatesWritesMode(t *testing.T) {
	cfg := config.Defaults()
	applyConfigUpdates(cfg, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/a", "mode": "edit"},
	}})
	if len(cfg.Notifications) != 1 {
		t.Fatalf("notifications = %d entries, want 1", len(cfg.Notifications))
	}
	if cfg.Notifications[0].Mode != "edit" {
		t.Errorf("Mode = %q, want edit", cfg.Notifications[0].Mode)
	}
}

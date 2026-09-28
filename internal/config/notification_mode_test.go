package config

import "testing"

// TestNotificationModeValidation: "edit" and "separate" are the vocabulary;
// empty means separate. Anything else is normalised back to separate, because
// a typo must never silently switch a channel to one-message-per-job.
func TestNotificationModeValidation(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
		want    string
	}{
		{"", false, ""},
		{"separate", false, "separate"},
		{"edit", false, "edit"},
		{"Edit", true, "separate"},
		{"editing", true, "separate"},
		{"none", true, "separate"},
	} {
		cfg := Defaults()
		cfg.Notifications = []NotificationConfig{{URL: "discord://1/a", Mode: tc.in}}
		errs := Validate(cfg)
		var found bool
		for _, e := range errs {
			if containsMode(e.Error()) {
				found = true
			}
		}
		if found != tc.wantErr {
			t.Errorf("mode %q: reported error = %v, want %v (errs=%v)", tc.in, found, tc.wantErr, errs)
		}

		norm := Defaults()
		norm.Notifications = []NotificationConfig{{URL: "discord://1/a", Mode: tc.in}}
		Normalize(norm)
		if norm.Notifications[0].Mode != tc.want {
			t.Errorf("mode %q normalised to %q, want %q", tc.in, norm.Notifications[0].Mode, tc.want)
		}
	}
}

func containsMode(s string) bool {
	for i := 0; i+len("notifications[0].mode") <= len(s); i++ {
		if s[i:i+len("notifications[0].mode")] == "notifications[0].mode" {
			return true
		}
	}
	return false
}

package routes

import "testing"

// PUT /api/config applies config.Validate's platform rule: an unknown
// platform is refused on an enabled channel, and let through on the disabled
// channel a hand-edited file leaves behind (owner ruling R2-M2), which the
// settings form sends back with every save.
//
// Mutant: refuse the platform whatever enabled says — the disabled entry 400s
// every save.
func TestValidateConfigUpdatesChannelPlatform(t *testing.T) {
	entry := func(platform string, enabled any) map[string]any {
		obj := map[string]any{"id": "somebody", "platform": platform}
		if enabled != nil {
			obj["enabled"] = enabled
		}
		return obj
	}
	const key = "channels[0].platform"
	for _, tc := range []struct {
		name    string
		obj     map[string]any
		refused bool
	}{
		{"unknown, disabled", entry("kick", false), false},
		{"unknown, enabled", entry("kick", true), true},
		{"unknown, enabled by default", entry("kick", nil), true},
		{"wrong case", entry("Twitch", true), true},
		{"twitch", entry("twitch", true), false},
		{"empty", entry("", nil), false},
	} {
		errs := validateConfigUpdates(map[string]any{"channels": []any{tc.obj}})
		if _, got := errs[key]; got != tc.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", tc.name, got, tc.refused, errs)
		}
	}
}

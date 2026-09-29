package youtube

import "testing"

// TestGvsTokenRequired pins GvsTokenRequired — the one GVS PO-token policy
// question every download path asks — for every Format.Source label the
// extraction cascade stamps (collectFormats and the client table in
// player_api_strategy.go), plus the empty and unknown labels, which must both
// answer false: a label nobody recognises gets no token.
func TestGvsTokenRequired(t *testing.T) {
	cases := []struct {
		source      string
		gvsRequired bool
	}{
		{"watch_page", true},
		{"web", true},
		{"web_safari", true},
		{"web_creator", true},
		{"web_embedded", false},
		{"tv_auth", false},
		{"tv_public", false},
		{"visionos", false},
		{"android_vr", false},
		{"android_vr_dash_fallback", false},
		{"", false},
		{"bogus", false},
	}
	for _, tc := range cases {
		if got := GvsTokenRequired(tc.source); got != tc.gvsRequired {
			t.Errorf("GvsTokenRequired(%q) = %v, want %v", tc.source, got, tc.gvsRequired)
		}
	}
}

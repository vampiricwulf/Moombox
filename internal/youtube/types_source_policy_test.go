package youtube

import "testing"

// TestSourcePolicyPredicates pins IsWebPOSource and GvsTokenRequired for every
// Format.Source label the extraction cascade stamps (collectFormats and the
// client table in player_api_strategy.go), plus the empty and unknown labels,
// which must both answer false — a label nobody recognises gets no token.
func TestSourcePolicyPredicates(t *testing.T) {
	cases := []struct {
		source      string
		webPO       bool
		gvsRequired bool
	}{
		{"watch_page", true, true},
		{"web", true, true},
		{"web_safari", true, true},
		{"web_creator", true, true},
		{"web_embedded", true, false},
		{"tv_auth", true, false},
		{"tv_public", true, false},
		{"visionos", false, false},
		{"android_vr", false, false},
		{"android_vr_dash_fallback", false, false},
		{"", false, false},
		{"bogus", false, false},
	}
	for _, tc := range cases {
		if got := IsWebPOSource(tc.source); got != tc.webPO {
			t.Errorf("IsWebPOSource(%q) = %v, want %v", tc.source, got, tc.webPO)
		}
		if got := GvsTokenRequired(tc.source); got != tc.gvsRequired {
			t.Errorf("GvsTokenRequired(%q) = %v, want %v", tc.source, got, tc.gvsRequired)
		}
	}
}

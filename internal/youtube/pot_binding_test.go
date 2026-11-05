package youtube

import (
	"os"
	"testing"
)

// TestGvsContentBinding pins yt-dlp's three-way rule
// (get_webpo_content_binding): experiment → videoID, authenticated →
// datasyncID, otherwise visitorData. Moombox hardcoded the first branch until
// 2026-08-15; these cases exist so a future reader can see the rule is
// conditional, not a constant.
//
// wantGuess is gvsContentBinding's verdict that the binding is a guess (owner
// decision D12): signed in, no experiment, no datasync ID — the session
// upstream mints nothing for. Mutants (each run): reporting no guess fails
// the two signed-in fallback rows; reporting one for a signed-out visitor
// binding fails the logged-out row; reporting one under the experiment fails
// its datasync and signed-out rows; reporting none under the experiment
// without a datasync ID, signed in, fails that row (wave FC decision 6:
// upstream's datasync refusal has no experiment exemption).
func TestGvsContentBinding(t *testing.T) {
	tests := []struct {
		name        string
		ytcfg       *YtcfgData
		auth        bool
		wantBinding string
		wantKind    string
		wantGuess   bool
	}{
		{
			name:        "experiment wins over everything",
			ytcfg:       &YtcfgData{GvsBindToVideoID: true, DataSyncID: "ds", VisitorData: "vd"},
			auth:        true,
			wantBinding: "vid123",
			wantKind:    BindingVideoID,
		},
		{
			// Upstream's GVS refusal for a signed-in session with no datasync
			// ID has no experiment exemption (fetch_po_token,
			// _video.py:2815-2819), so the experiment's binding is a guess
			// there (wave FC decision 6).
			name:        "experiment signed in without datasync is a guess",
			ytcfg:       &YtcfgData{GvsBindToVideoID: true, VisitorData: "vd"},
			auth:        true,
			wantBinding: "vid123",
			wantKind:    BindingVideoID,
			wantGuess:   true,
		},
		{
			name:        "experiment signed out is no guess",
			ytcfg:       &YtcfgData{GvsBindToVideoID: true, VisitorData: "vd"},
			auth:        false,
			wantBinding: "vid123",
			wantKind:    BindingVideoID,
		},
		{
			name:        "authenticated without experiment binds to datasync",
			ytcfg:       &YtcfgData{DataSyncID: "ds", VisitorData: "vd"},
			auth:        true,
			wantBinding: "ds",
			wantKind:    BindingDataSyncID,
		},
		{
			name:        "logged out binds to visitor data",
			ytcfg:       &YtcfgData{VisitorData: "vd"},
			auth:        false,
			wantBinding: "vd",
			wantKind:    BindingVisitorData,
		},
		{
			name:        "authenticated but datasync missing falls to visitor data",
			ytcfg:       &YtcfgData{VisitorData: "vd"},
			auth:        true,
			wantBinding: "vd",
			wantKind:    BindingVisitorData,
			wantGuess:   true,
		},
		{
			name:        "nothing session-scoped falls back to videoID",
			ytcfg:       &YtcfgData{},
			auth:        true,
			wantBinding: "vid123",
			wantKind:    BindingVideoID,
			wantGuess:   true,
		},
		{
			name:        "nil ytcfg still yields a binding",
			ytcfg:       nil,
			auth:        false,
			wantBinding: "vid123",
			wantKind:    BindingVideoID,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := GvsContentBinding("vid123", tc.ytcfg, tc.auth, "chan456")
			if got != tc.wantBinding || kind != tc.wantKind {
				t.Errorf("got (%q,%q), want (%q,%q)", got, kind, tc.wantBinding, tc.wantKind)
			}
			if _, _, guessed := gvsContentBinding("vid123", tc.ytcfg, tc.auth, "chan456"); guessed != tc.wantGuess {
				t.Errorf("guessed = %v, want %v", guessed, tc.wantGuess)
			}
		})
	}

	// Last-resort channel fallback when even the video ID is unavailable.
	if got, kind := GvsContentBinding("", &YtcfgData{}, false, "chan456"); got != "chan456" || kind != BindingChannelID {
		t.Errorf("channel fallback: got (%q,%q)", got, kind)
	}
	if _, kind, guessed := gvsContentBinding("", &YtcfgData{}, true, "chan456"); kind != BindingChannelID || !guessed {
		t.Errorf("signed-in channel fallback: kind %q guessed %v, want %q guessed", kind, guessed, BindingChannelID)
	}
}

// TestGvsBindVideoIDRegex covers the escaped form YouTube actually ships
// (verified against a live watch page 2026-08-15: the flag appears as
// html5_generate_content_po_token\u003dtrue inside serializedExperimentFlags)
// as well as the plain form, and confirms an explicitly-false flag does not
// match.
func TestGvsBindVideoIDRegex(t *testing.T) {
	cases := map[string]bool{
		`"serializedExperimentFlags":"a=b\u0026html5_generate_content_po_token\u003dtrue\u0026c=d"`: true,
		`html5_generate_content_po_token=true`:                                                      true,
		`html5_generate_content_po_token\u003dfalse`:                                                false,
		`html5_generate_content_po_token=false`:                                                     false,
		`no flag here`:                                                                              false,
	}
	for in, want := range cases {
		if got := gvsBindVideoIDRegex.MatchString(in); got != want {
			t.Errorf("MatchString(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestGvsBindVideoIDAgainstLivePage runs the detector over a real watch page
// when one has been captured, so the escaping assumption above is checked
// against YouTube rather than against our own fixture. Skips when absent.
func TestGvsBindVideoIDAgainstLivePage(t *testing.T) {
	const fixture = `C:\Users\Wulf\AppData\Local\Temp\claude\D--Git-Moombox\90a4e150-ec10-4219-bd59-c233e8508d1b\scratchpad\yt_page.html`
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Skip("no captured watch page available")
	}
	if !gvsBindVideoIDRegex.MatchString(string(b)) {
		t.Error("live page carries html5_generate_content_po_token=true but the detector missed it")
	}
}

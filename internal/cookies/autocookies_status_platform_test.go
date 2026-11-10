package cookies

import (
	"encoding/json"
	"strings"
	"testing"
)

// GetStatus names the platform of the setup in progress, so a dashboard that
// adopts a setup after a reload knows which platform it is finishing instead
// of guessing it from the dialog's label (K33). Nothing while no setup is in
// progress, and absent from the JSON then.
//
// Mutant: SetupPlatform filled from targetPlatform unconditionally — the
// idle row reports a platform.
func TestStatusNamesTheSetupPlatformOnlyWhileInProgress(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	s.mu.Lock()
	s.setupClaimed = true
	s.targetPlatform = "twitch"
	s.mu.Unlock()
	st := s.GetStatus()
	if !st.SetupInProgress || st.SetupPlatform != "twitch" {
		t.Fatalf("in progress: SetupInProgress=%v SetupPlatform=%q, want true/twitch", st.SetupInProgress, st.SetupPlatform)
	}

	s.mu.Lock()
	s.setupClaimed = false
	s.mu.Unlock()
	st = s.GetStatus()
	if st.SetupInProgress || st.SetupPlatform != "" {
		t.Errorf("idle: SetupInProgress=%v SetupPlatform=%q, want false/empty", st.SetupInProgress, st.SetupPlatform)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "setupPlatform") {
		t.Errorf("an idle status carries setupPlatform: %s", raw)
	}
}

package worker

import "testing"

func TestVodRefreshDecision(t *testing.T) {
	cases := []struct {
		name                       string
		behindHead, progressed     bool
		attempt                    int
		manifestlessStillAvailable bool
		want                       bool
	}{
		{"incomplete with progress refreshes", true, true, 1, true, true},
		{"complete finalize stops", false, true, 1, true, false},
		{"no progress stops (avoid API spin)", true, false, 1, true, false},
		{"attempts exhausted stops", true, true, maxVodRefreshAttempts, true, false},
		{"stream became true VOD stops", true, true, 1, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldRefreshVodDownload(c.behindHead, c.progressed, c.attempt, c.manifestlessStillAvailable)
			if got != c.want {
				t.Errorf("shouldRefreshVodDownload = %v, want %v", got, c.want)
			}
		})
	}
}

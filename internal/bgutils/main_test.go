package bgutils

import (
	"net/http"
	"os"
	"testing"
)

// TestMain keeps the package's unit tests off the network. Several of them
// drive the goja mint flow only to check what it does to the caches, and
// accepted either outcome ("may succeed or fail") — so they called Google's
// real endpoints, taking up to half a minute each and leaving the suite's
// running time to the network. Every BotGuard request goes through
// bgHTTPClient, which now refuses at once unless MOOMBOX_LIVE_BG_TEST=1 asks
// for the live checks. A test that needs a particular answer still installs
// its own client and restores this one.
func TestMain(m *testing.M) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") != "1" {
		bgHTTPClient = &http.Client{Transport: refusingTransport{}}
	}
	os.Exit(m.Run())
}

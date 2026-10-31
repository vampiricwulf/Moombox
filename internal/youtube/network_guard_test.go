package youtube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// networkGuardEnv is what the guard sets for the suite: every proxy variable
// pointed at the tripwire, and every variable that would exempt a host from
// it (or make net/http ignore HTTP_PROXY, as REQUEST_METHOD does under CGI)
// cleared.
var (
	networkGuardProxyVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
	networkGuardClearVars = []string{"NO_PROXY", "no_proxy", "REQUEST_METHOD"}
)

// productionIframePlayerURL is fetchIframePlayerURL as the package
// initialised it, before TestMain stubbed it.
var productionIframePlayerURL func(context.Context, string) (string, error)

// productionPlayerRetryBackoffBase is playerRetryBackoffBase as the package
// initialised it, before TestMain scaled it down.
var productionPlayerRetryBackoffBase time.Duration

// TestMain keeps the unit suite off the network. Every fetch in this package
// goes out through the shared httpx transport (apiClient, utils.FetchBody) or
// a client a test built, and each takes its proxy from the environment, which
// is never asked for a loopback host. So the suite runs behind a proxy that
// answers nothing (502) and records what reached it: the stubs (httptest
// servers on 127.0.0.1) are untouched, and a test that reaches past them —
// the homepage read through constants.YouTubeURLs.Base that a probe now
// makes, say — fails the run. Otherwise it downloads the live page on every
// CI run, carrying the test's fake credentials, and a runner whose egress is
// blackholed waits out the fetch's timeout. The live tests
// (MOOMBOX_LIVE_YT_TEST, MOOMBOX_LIVE_YT_COOKIES) run without it. Either way
// the tests run through treeguard.Run, which fails a run that left a file in
// the package directory.
//
// The player-URL fallback (fetchIframePlayerURL) answers nothing in the unit
// suite, as a blackholed iframe API would: every cascade test whose stubbed
// watch page names no player would otherwise reach for /iframe_api. The
// tests that are about that fallback set it themselves, and
// productionIframePlayerURL keeps what production runs for the test that
// pins it.
//
// The player calls' retry ladder (playerRetryBackoffBase, 1 s, 2 s, 4 s) runs
// at a millisecond base. A 404 is retried since G46, as upstream retries it,
// and the stubbed transports answer 404 for every client a test does not
// script: at the production base each such client cost the suite 7 s, some
// seventy-five times over. The tests about the ladder's timing set their own
// base (scaleRetryBackoff), and TestPlayerRetryBackoffBaseIsOneSecond pins
// the production one. The live suite runs the stubbed tests too, so it is
// scaled there as well — ahead of its early return, which left the pin
// reading zero and the suite near go test's ten-minute limit; a live call
// a server fails is retried within milliseconds, which is no worse for a
// test.
func TestMain(m *testing.M) {
	productionPlayerRetryBackoffBase = playerRetryBackoffBase
	playerRetryBackoffBase = time.Millisecond
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") == "1" || os.Getenv("MOOMBOX_LIVE_YT_COOKIES") != "" {
		os.Exit(treeguard.Run(m))
	}
	productionIframePlayerURL = fetchIframePlayerURL
	fetchIframePlayerURL = func(context.Context, string) (string, error) {
		return "", errors.New("the unit suite's iframe API answers nothing")
	}

	var mu sync.Mutex
	reached := map[string]int{}
	trip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached[r.Method+" "+r.Host]++
		mu.Unlock()
		http.Error(w, "the youtube unit suite does not reach the network", http.StatusBadGateway)
	}))
	for _, k := range networkGuardProxyVars {
		if err := os.Setenv(k, trip.URL); err != nil {
			fmt.Fprintf(os.Stderr, "network guard: set %s: %v\n", k, err)
			os.Exit(1)
		}
	}
	for _, k := range networkGuardClearVars {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintf(os.Stderr, "network guard: unset %s: %v\n", k, err)
			os.Exit(1)
		}
	}

	code := treeguard.Run(m)
	trip.Close()

	if len(reached) > 0 {
		lines := make([]string, 0, len(reached))
		for req, n := range reached {
			lines = append(lines, fmt.Sprintf("  %s (%d×)", req, n))
		}
		slices.Sort(lines)
		fmt.Fprintln(os.Stderr, "FAIL: the youtube unit suite reached the network:")
		for _, l := range lines {
			fmt.Fprintln(os.Stderr, l)
		}
		fmt.Fprintln(os.Stderr, "Stub the fetch (stubHomepage, recordPlayerCalls, fetchWatchPage, an httptest server) "+
			"or gate the test behind MOOMBOX_LIVE_YT_TEST=1; -run narrows down which test made it.")
		code = 1
	}
	os.Exit(code)
}

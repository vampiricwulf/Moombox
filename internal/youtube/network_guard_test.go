package youtube

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
)

// networkGuardEnv is what the guard sets for the suite: every proxy variable
// pointed at the tripwire, and every variable that would exempt a host from
// it (or make net/http ignore HTTP_PROXY, as REQUEST_METHOD does under CGI)
// cleared.
var (
	networkGuardProxyVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
	networkGuardClearVars = []string{"NO_PROXY", "no_proxy", "REQUEST_METHOD"}
)

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
// (MOOMBOX_LIVE_YT_TEST, MOOMBOX_LIVE_YT_COOKIES) run without it.
func TestMain(m *testing.M) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") == "1" || os.Getenv("MOOMBOX_LIVE_YT_COOKIES") != "" {
		os.Exit(m.Run())
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

	code := m.Run()
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

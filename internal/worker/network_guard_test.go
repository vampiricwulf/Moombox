package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"

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

// TestMain keeps the unit suite off the network, the way the youtube
// package's guard does. Every fetch here goes out through workerHTTPClient
// (the shared httpx transport) or a client a test built, and each takes its
// proxy from the environment, which is never asked for a loopback host. So
// the suite runs behind a proxy that answers nothing (502) and records what
// reached it: the stubs (httptest servers on 127.0.0.1) are untouched, and a
// test that reaches past them fails the run.
//
// ytThumbnailBase is pointed at a stub that answers 404 for every size. Each
// test that runs a YouTube job through the mux finalize reaches copyAssets,
// which fetched all five thumbnail sizes from i.ytimg.com: ~170 requests a
// run, for video IDs that do not exist, so none ever came back and no test
// saved a thumbnail. The 404 keeps that outcome without the round trips; a
// test that wants a thumbnail serves its own (TestCopyAssetsTakesTheFirstUsableThumbnailSize).
//
// The tests run through treeguard.Run, which fails a run that left a file in
// the package directory.
func TestMain(m *testing.M) {
	var mu sync.Mutex
	reached := map[string]int{}
	trip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached[r.Method+" "+r.Host]++
		mu.Unlock()
		http.Error(w, "the worker unit suite does not reach the network", http.StatusBadGateway)
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

	thumbs := httptest.NewServer(http.NotFoundHandler())
	ytThumbnailBase = thumbs.URL

	code := treeguard.Run(m)
	thumbs.Close()
	trip.Close()

	if len(reached) > 0 {
		lines := make([]string, 0, len(reached))
		for req, n := range reached {
			lines = append(lines, fmt.Sprintf("  %s (%d×)", req, n))
		}
		slices.Sort(lines)
		fmt.Fprintln(os.Stderr, "FAIL: the worker unit suite reached the network:")
		for _, l := range lines {
			fmt.Fprintln(os.Stderr, l)
		}
		fmt.Fprintln(os.Stderr, "Stub the fetch (ytThumbnailBase, an httptest server); -run narrows down which test made it.")
		code = 1
	}
	os.Exit(code)
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nodeIndexURL lists every Node.js release, newest first, each with a
// `security` flag that is true for a security release.
const nodeIndexURL = "https://nodejs.org/dist/index.json"

// The check's three outcomes, as process exit codes. A fetch that failed is
// not a finding: nodejs.org being unreachable for a minute must not turn the
// weekly scan red, so it warns and passes.
const (
	securityCheckOK      = 0
	securityCheckFailed  = 1
	securityCheckWarning = 0
)

// nodeRelease is the part of one index.json entry the check reads.
type nodeRelease struct {
	Version  string `json:"version"`
	Security bool   `json:"security"`
}

// parseNodeVersion reads "v24.22.2" into its three numbers.
func parseNodeVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if !strings.HasPrefix(v, "v") || len(parts) != 3 {
		return out, fmt.Errorf("not a Node version: %q", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("not a Node version: %q", v)
		}
		out[i] = n
	}
	return out, nil
}

// versionLess reports whether a is an older version than b.
func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// newerSecurityReleases returns the security releases in index (the body of
// nodeIndexURL) that are on pin's major line and newer than pin, oldest
// first. It is an error when index is not a release list or does not hold
// pin itself: the check then cannot tell whether anything is newer, and
// saying "nothing found" would be a guess.
func newerSecurityReleases(index []byte, pin string) ([]string, error) {
	pinned, err := parseNodeVersion(pin)
	if err != nil {
		return nil, err
	}
	var releases []nodeRelease
	if err := json.Unmarshal(index, &releases); err != nil {
		return nil, fmt.Errorf("the release index is not a JSON list: %w", err)
	}
	type hit struct {
		v    [3]int
		name string
	}
	var hits []hit
	sawPin := false
	for _, r := range releases {
		v, err := parseNodeVersion(r.Version)
		if err != nil || v[0] != pinned[0] {
			continue
		}
		if v == pinned {
			sawPin = true
		}
		if r.Security && versionLess(pinned, v) {
			hits = append(hits, hit{v, r.Version})
		}
	}
	if !sawPin {
		return nil, fmt.Errorf("the release index does not list the pinned %s", pin)
	}
	sort.Slice(hits, func(i, j int) bool { return versionLess(hits[i].v, hits[j].v) })
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.name
	}
	return names, nil
}

// fetchNodeIndex downloads the release index. The file is a few hundred
// kilobytes; the cap is there for a redirect to something else.
func fetchNodeIndex() ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(nodeIndexURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// checkSecurity is `fetch-node -check-security`: it fails when Node has
// published a security release on the pinned major line that is newer than
// the pin, and downloads nothing else. The weekly vulnerability scan runs it
// (.github/workflows/vuln-scan.yml); no build, pull request or release does.
// The `::warning::` and `::error::` prefixes are what GitHub Actions turns
// into annotations on the run.
func checkSecurity(pin string, fetch func() ([]byte, error), out io.Writer) int {
	index, err := fetch()
	if err != nil {
		fmt.Fprintf(out, "::warning::Node pin check skipped: could not read %s (%v). Pinned: %s.\n", nodeIndexURL, err, pin)
		return securityCheckWarning
	}
	newer, err := newerSecurityReleases(index, pin)
	if err != nil {
		fmt.Fprintf(out, "::error::Node pin check could not judge the pin: %v\n", err)
		return securityCheckFailed
	}
	if len(newer) == 0 {
		fmt.Fprintf(out, "fetch-node: no security release newer than the pinned %s on its major line\n", pin)
		return securityCheckOK
	}
	fmt.Fprintf(out, "::error::Node %s is pinned, and a newer security release exists: %s. Bump nodeVersion in tools/fetch-node/main.go (steps at the top of that file).\n",
		pin, strings.Join(newer, ", "))
	return securityCheckFailed
}

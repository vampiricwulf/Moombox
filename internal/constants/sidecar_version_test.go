package constants

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// serverJSClientVersionPattern anchors to the START of a line (multiline
// mode) and requires the exact `const CLIENT_VERSION = "...";` declaration
// form. This is a text pin, not an execution pin — see
// TestSidecarClientVersionMatchesConstants's doc comment for why, and for
// the limitation that shape carries.
var serverJSClientVersionPattern = regexp.MustCompile(`(?m)^const CLIENT_VERSION = "([^"]*)";$`)

// TestSidecarClientVersionMatchesConstants pins bgutil-sidecar/src/server.js's
// CLIENT_VERSION literal to constants.WebClient.ClientVersion — the single
// source of truth the three WEB-family clients' internal-consistency tests
// above (TestWebClientInternallyConsistent and its two siblings) already pin
// themselves to. Without this, the JS sidecar's copy of the Innertube WEB
// client version can drift from the Go client's the day one side is bumped
// and the other is not: YouTube would see a contradictory fingerprint from
// requests the same logical session makes through two different code paths
// (the sidecar's BotGuard/PO-token minting vs. every other WEB request Go
// sends directly).
//
// LIMITATION — this is a text pin, not an execution pin, and it is weaker
// than that distinction usually implies. server.js is an ES module that
// imports jsdom and bgutils-js and bootstraps a JSDOM window as a
// module-level side effect (see the file's own "One-time DOM bootstrap"
// comment); it cannot be loaded into goja far enough to read the constant
// without actually starting the sidecar process, unlike
// web/public/modules/utils.js's dependency-free functions
// (cookies_setup_utilsvm_test.go's approach). So this test only proves that
// ONE line matching the exact `const CLIENT_VERSION = "...";` shape, anchored
// to line start, holds the same value as the Go constant. It would not catch
// the declaration being renamed and every call site repointed at a
// differently-named constant carrying a stale value — the standing rule that
// a name/substring check is no guard when the decoy can rename still applies
// to what is checked here; anchoring the line-start and exact-form and
// exactly-one-match constraints is as strict as a non-executing pin can get,
// not a way around that limitation.
func TestSidecarClientVersionMatchesConstants(t *testing.T) {
	path := filepath.Join("..", "..", "bgutil-sidecar", "src", "server.js")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	matches := serverJSClientVersionPattern.FindAllStringSubmatch(string(src), -1)
	switch len(matches) {
	case 0:
		t.Fatalf("no line in %s matches `const CLIENT_VERSION = \"...\";` at line start — "+
			"the pin can no longer see the declaration (renamed? reformatted?)", path)
	case 1:
		// exactly one candidate — proceed.
	default:
		t.Fatalf("%d lines in %s match the CLIENT_VERSION declaration pattern, want exactly 1 — "+
			"the pin cannot tell which one is authoritative", len(matches), path)
	}

	got := matches[0][1]
	want := WebClient.ClientVersion
	if got != want {
		t.Errorf("bgutil-sidecar/src/server.js CLIENT_VERSION = %q, constants.WebClient.ClientVersion = %q — "+
			"the JS sidecar and the Go WEB client have drifted; bump both together", got, want)
	}
}

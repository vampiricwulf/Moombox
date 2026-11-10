package youtube

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The two declarations of the interpreter gate in bgutil-sidecar/src/server.js,
// as text. Both are anchored to a whole line and to the exact form the file
// uses today, so a reformatted or renamed declaration fails the test instead
// of passing unseen.
var (
	// serverJSInterpreterHostsRe captures the body of the
	// `const ALLOWED_INTERPRETER_HOSTS = [ ... ];` array literal.
	serverJSInterpreterHostsRe = regexp.MustCompile(`(?ms)^const ALLOWED_INTERPRETER_HOSTS = \[\n(.*?)^\];$`)
	// serverJSInterpreterHostLineRe is one element line of that literal.
	serverJSInterpreterHostLineRe = regexp.MustCompile(`^\s+"([^"]+)",$`)
	// serverJSStaticScriptPathRe captures the source of the regex literal
	// assertGoogleHost tests the interpreter URL's pathname against.
	serverJSStaticScriptPathRe = regexp.MustCompile(`(?m)^\s*if \(!/(.+)/\.test\(parsed\.pathname\)\) \{$`)
)

// TestInterpreterGateMatchesSidecar pins the two copies of the BotGuard
// interpreter gate to each other: allowedInterpreterHosts and
// staticScriptPathRe in watch_page.go, and ALLOWED_INTERPRETER_HOSTS and the
// pathname regex of assertGoogleHost in bgutil-sidecar/src/server.js. The
// sidecar executes the interpreter it fetches, so the gate is a
// code-execution boundary; both files say "keep in sync", and this test is
// what fails when one side is edited alone.
//
// This is a text pin, not an execution pin (server.js cannot be loaded
// without starting the sidecar; see
// internal/constants/sidecar_version_test.go). It proves the two declarations
// hold the same hosts and the same pattern. It does not prove either side
// still applies them: TestAllowedInterpreterHosts and the sidecar's own node
// tests cover that.
//
// It skips only when bgutil-sidecar/ is not beside this package. With the
// file present, a declaration the patterns above cannot find is a failure.
//
// Mutants: a host added to, removed from or respelled in either list; a
// character changed in either regex; either JS declaration renamed or
// reformatted.
func TestInterpreterGateMatchesSidecar(t *testing.T) {
	const goFile = "internal/youtube/watch_page.go"
	const jsFile = "bgutil-sidecar/src/server.js"

	sidecarDir := filepath.Join("..", "..", "bgutil-sidecar")
	if _, err := os.Stat(sidecarDir); errors.Is(err, fs.ErrNotExist) {
		t.Skipf("bgutil-sidecar/ is not beside this package (%v); nothing to compare against", err)
	}
	raw, err := os.ReadFile(filepath.Join(sidecarDir, "src", "server.js"))
	if err != nil {
		t.Fatalf("bgutil-sidecar/ is present but %s could not be read: %v", jsFile, err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	t.Run("hosts", func(t *testing.T) {
		blocks := serverJSInterpreterHostsRe.FindAllStringSubmatch(src, -1)
		if len(blocks) != 1 {
			t.Fatalf("%d declarations in %s match `const ALLOWED_INTERPRETER_HOSTS = [ ... ];` at line start, want exactly 1 — "+
				"the pin can no longer see the list (renamed? reformatted?)", len(blocks), jsFile)
		}
		var jsHosts []string
		for _, line := range strings.Split(blocks[0][1], "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			m := serverJSInterpreterHostLineRe.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("line %q inside ALLOWED_INTERPRETER_HOSTS in %s is not a plain `\"host\",` element — "+
					"the pin cannot read the list", line, jsFile)
			}
			jsHosts = append(jsHosts, m[1])
		}

		goSorted := slices.Sorted(slices.Values(allowedInterpreterHosts))
		jsSorted := slices.Sorted(slices.Values(jsHosts))
		if dup := len(jsSorted) - len(slices.Compact(slices.Clone(jsSorted))); dup != 0 {
			t.Errorf("ALLOWED_INTERPRETER_HOSTS in %s holds %d duplicate host(s): %q", jsFile, dup, jsHosts)
		}
		if dup := len(goSorted) - len(slices.Compact(slices.Clone(goSorted))); dup != 0 {
			t.Errorf("allowedInterpreterHosts in %s holds %d duplicate host(s): %q", goFile, dup, allowedInterpreterHosts)
		}
		if !slices.Equal(goSorted, jsSorted) {
			t.Errorf("the interpreter host lists differ:\n  %s allowedInterpreterHosts    = %q\n  %s ALLOWED_INTERPRETER_HOSTS = %q\n"+
				"the two are one gate; change both together", goFile, goSorted, jsFile, jsSorted)
		}
	})

	t.Run("static script path", func(t *testing.T) {
		matches := serverJSStaticScriptPathRe.FindAllStringSubmatch(src, -1)
		if len(matches) != 1 {
			t.Fatalf("%d lines in %s match `if (!/.../.test(parsed.pathname)) {`, want exactly 1 — "+
				"the pin can no longer see the pathname rule (renamed? reformatted?)", len(matches), jsFile)
		}
		// A JS regex literal escapes its own delimiter; Go's source does not.
		got := strings.ReplaceAll(matches[0][1], `\/`, `/`)
		if want := staticScriptPathRe.String(); got != want {
			t.Errorf("the static-script path rules differ:\n  %s staticScriptPathRe = %s\n  %s pathname regex    = %s\n"+
				"the two are one gate; change both together", goFile, want, jsFile, got)
		}
	})
}

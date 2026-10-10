package notifications

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// backtickedEvent is one `event_name` in a doc list.
var backtickedEvent = regexp.MustCompile("`([a-z_]+)`")

// TestTheDocsNameEveryEventAndEveryClose holds the two hand-written lists a
// reader takes the vocabulary from to the registries they describe: SPEC.md's
// "Notification events" line against EventGroups, and operations.md's list of
// closes against closeEvents. auth_recovered joined both registries and
// neither list, so SPEC.md said the event did not exist and operations.md
// left it out of the closes that are never pinged through their alert's
// mention.
//
// Mutants: drop `auth_recovered` from either list, or add a close to
// closeEvents (or an event to EventGroups) without documenting it.
func TestTheDocsNameEveryEventAndEveryClose(t *testing.T) {
	spec, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(spec), "\n") {
		if strings.HasPrefix(l, "**Notification events:**") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("SPEC.md has no **Notification events:** line — re-anchor this test rather than deleting it")
	}
	// The list is everything before the first " — "; the prose after it names
	// a retired key on purpose.
	listed := map[string]bool{}
	for _, m := range backtickedEvent.FindAllStringSubmatch(strings.SplitN(line, " — ", 2)[0], -1) {
		listed[m[1]] = true
	}
	for _, g := range EventGroups {
		for _, e := range g.Events {
			if !listed[e] {
				t.Errorf("SPEC.md's Notification events list leaves out %q (group %q)", e, g.Name)
			}
		}
	}

	ops, err := os.ReadFile("../../docs/spec/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("A close \\(([^)]*) — `closeEvents`").FindSubmatch(ops)
	if m == nil {
		t.Fatal("operations.md has no \"A close (… — `closeEvents`\" list — re-anchor this test rather than deleting it")
	}
	closes := map[string]bool{}
	for _, mm := range backtickedEvent.FindAllSubmatch(m[1], -1) {
		closes[string(mm[1])] = true
	}
	for e := range closeEvents {
		if !closes[e] {
			t.Errorf("operations.md's list of closes leaves out %q", e)
		}
	}
	for e := range closes {
		if !closeEvents[e] {
			t.Errorf("operations.md lists %q as a close, and closeEvents does not", e)
		}
	}
}

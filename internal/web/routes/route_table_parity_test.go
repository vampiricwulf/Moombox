package routes

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// routeRegistrationRe finds a chi registration — r.Get("/api/jobs"),
// r.With(mw).Post("/api/import"), heavy.Post("/api/cookies/import"). The
// leading "/" is what separates a route path from req.Header.Get("Origin")
// and req.URL.Query().Get("v"), which share the method names exactly.
var routeRegistrationRe = regexp.MustCompile(`\.(Get|Post|Put|Delete|Patch|Head)\("(/[^"]*)"`)

// routeTableRowRe finds one row of the REST catalog in user-interfaces.md:
// a method cell followed by a path cell. Three- and four-column tables (the
// rate-limit column) both match, because only the first two cells are read.
var routeTableRowRe = regexp.MustCompile("^[|] `(GET|POST|PUT|DELETE|PATCH|HEAD)` [|] `(/[^`]*)` [|]")

// repoRootFromTest walks up from the test's working directory to the module
// root, so the scan does not depend on where `go test` was invoked.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// registeredRoutes is every "METHOD /path" the binary actually serves, read
// out of the sources under internal/ and cmd/ (tests excluded — a test server
// registering a fixture route is not a documented endpoint).
func registeredRoutes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for i, line := range strings.Split(string(b), "\n") {
				for _, m := range routeRegistrationRe.FindAllStringSubmatch(line, -1) {
					out[strings.ToUpper(m[1])+" "+m[2]] = fmt.Sprintf("%s:%d", rel, i+1)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return out
}

// documentedRoutes is every route row in the spec's REST catalog.
func documentedRoutes(t *testing.T, root string) map[string]int {
	t.Helper()
	p := filepath.Join(root, "docs", "spec", "user-interfaces.md")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	out := map[string]int{}
	for i, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if m := routeTableRowRe.FindStringSubmatch(line); m != nil {
			out[m[1]+" "+m[2]] = i + 1
		}
	}
	return out
}

// TestRouteTableMatchesRegisteredRoutes is the differential pin the route
// catalog never had: the table in docs/spec/user-interfaces.md and the set of
// routes the server registers must be the SAME set, in both directions.
//
// Prose review had missed eight routes — POST /api/jobs/{id}/resume,
// /reinitialize and /mux, GET /api/jobs/{id}/thumbnail, PUT
// /api/config/channels/reorder, POST /api/monitors/check-now, POST
// /api/notifications/test and GET /pot_stats — because nothing compared the
// two lists; "complete catalog" was a promise with no check behind it.
//
// MUTANT A: delete a row from the table — this names it as registered and
// undocumented. MUTANT B: add a row for a path nothing serves — this names it
// as documented and unregistered.
func TestRouteTableMatchesRegisteredRoutes(t *testing.T) {
	root := repoRootFromTest(t)
	registered := registeredRoutes(t, root)
	documented := documentedRoutes(t, root)

	if len(registered) < 50 {
		t.Fatalf("only %d registrations found — the source scan is not reaching the route files", len(registered))
	}

	var missing, extra []string
	for route, site := range registered {
		if _, ok := documented[route]; !ok {
			missing = append(missing, fmt.Sprintf("%s (registered at %s)", route, site))
		}
	}
	for route, lineNo := range documented {
		if _, ok := registered[route]; !ok {
			extra = append(extra, fmt.Sprintf("%s (user-interfaces.md:%d)", route, lineNo))
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	for _, m := range missing {
		t.Errorf("route registered but absent from the REST catalog in docs/spec/user-interfaces.md: %s", m)
	}
	for _, e := range extra {
		t.Errorf("route documented in docs/spec/user-interfaces.md but registered nowhere: %s", e)
	}
}

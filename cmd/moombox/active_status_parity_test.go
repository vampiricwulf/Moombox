package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// declaredJobStatuses reads every `JobStatus` constant out of the file that
// declares them, so the parity test below cannot be given a stale list.
//
// A hand-written slice here was the whole defect: adding a tenth status to
// internal/database/types.go and to the worker's active set left this test
// green, because the new status was in neither the slice nor the loop. The
// list is parsed rather than asked of internal/database, because the arc that
// added this verb is under a standing ruling that the DB layer stays untouched
// — an `AllJobStatuses()` helper there would break it for a test's benefit.
//
// go/parser, not a regexp: the const block carries doc comments and aligned
// values, and a regexp over it is a second parser with worse failure modes.
func declaredJobStatuses(t *testing.T) []database.JobStatus {
	t.Helper()

	const typesFile = "../../internal/database/types.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, typesFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", typesFile, err)
	}

	var out []database.JobStatus
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "JobStatus" {
				continue
			}
			for _, expr := range value.Values {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a JobStatus constant in %s is not a string literal (%T) — "+
						"this test reads the declared values, so teach it the new shape", typesFile, expr)
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s in %s: %v", lit.Value, typesFile, err)
				}
				out = append(out, database.JobStatus(unquoted))
			}
		}
	}
	return out
}

// TestActiveJobStatusListsAgree pins the one list that decides whether a job's
// staging directory is being written, and therefore whether set-aside recovery
// may touch it.
//
// worker.IsActiveJobStatus (internal/worker/orphans.go) is the source of
// truth: the worker and the REST route both refuse a recovery on it.
// tui.JobIsActive is a deliberate re-declaration, because internal/tui cannot
// import internal/worker (the import fence) — and nothing was comparing them.
// The dashboard's literal in web/public/modules/job-details.js is the third
// reader; its own jsdom test walks all four statuses plus Finished.
//
// Mutant: adding a status to one side, or dropping one — this names it. The
// list is DERIVED from types.go (declaredJobStatuses), so a tenth status
// cannot be skipped by forgetting to add it here as well.
func TestActiveJobStatusListsAgree(t *testing.T) {
	all := declaredJobStatuses(t)
	if len(all) < 9 {
		t.Fatalf("parsed %d JobStatus constants out of internal/database/types.go (%v), want at least the nine "+
			"the lifecycle declares — the parse, not the lists, is what broke", len(all), all)
	}
	active := 0
	for _, s := range all {
		w, ui := worker.IsActiveJobStatus(s), tui.JobIsActive(s)
		if w != ui {
			t.Errorf("status %q: worker.IsActiveJobStatus = %v but tui.JobIsActive = %v", s, w, ui)
		}
		if w {
			active++
		}
	}
	// A guard against the guard: if the worker's set ever emptied — or if the
	// parse above quietly missed one of the four — every row would agree
	// vacuously.
	if active != 4 {
		t.Errorf("worker.IsActiveJobStatus reports %d active statuses over the %d declared, want 4 (Upcoming, Live, Downloading, Muxing) — "+
			"if this list genuinely changed, update the dashboard literal in web/public/modules/job-details.js too", active, len(all))
	}
}

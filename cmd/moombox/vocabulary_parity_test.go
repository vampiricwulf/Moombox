package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// declaredStringConsts returns the values of every constant of type typeName
// declared in goFile, in declaration order. Parsed rather than listed, so a
// value added on the Go side without its JS twin fails the parity tests.
func declaredStringConsts(t *testing.T, goFile, typeName string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), goFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", goFile, err)
	}
	var out []string
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
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != typeName {
				continue
			}
			for _, expr := range value.Values {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a %s constant in %s is not a string literal (%T) — teach this test the new shape", typeName, goFile, expr)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s constants found in %s", typeName, goFile)
	}
	return out
}

// dashboardFrozenMap reads `export const NAME = Object.freeze({ key: "value", ... });`
// out of a dashboard module, keys bare or quoted. Fails when the object is
// not found, or holds an entry of another shape.
func dashboardFrozenMap(t *testing.T, jsFile, name string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(jsFile)
	if err != nil {
		t.Fatalf("read %s: %v", jsFile, err)
	}
	re := regexp.MustCompile(`(?s)export const ` + regexp.QuoteMeta(name) + ` = Object\.freeze\(\{(.*?)\}\);`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no `export const %s = Object.freeze({...});`", jsFile, name)
	}
	entryRe := regexp.MustCompile(`^"?([A-Za-z_][A-Za-z0-9_]*)"?\s*:\s*"([^"]*)"$`)
	out := map[string]string{}
	for _, entry := range strings.Split(string(m[1]), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		em := entryRe.FindStringSubmatch(entry)
		if em == nil {
			t.Fatalf("%s: %s entry %q is not `key: \"value\"` — teach this test the new shape", jsFile, name, entry)
		}
		out[em[1]] = em[2]
	}
	return out
}

// TestChatStatusSetMatchesTheDashboard: the details badge's variant table
// (utils.js CHAT_STATUS_VARIANTS) has exactly one entry per non-empty
// database.ChatStatus, and database.ChatStatuses lists every declared
// constant. Before, job-details.js held two hand copies of the table, both
// mapping an "error" nothing writes. The variants themselves are pinned too,
// so the badge colours stay what they were (and agree with the TUI's
// chatStatusColor: warning for incomplete, the in-progress colour for
// downloading, the finished colour for finished, neutral otherwise).
//
// Mutant: add a ChatStatus constant without a JS key (or the reverse), or
// re-add "error" — this names the value.
func TestChatStatusSetMatchesTheDashboard(t *testing.T) {
	declared := declaredStringConsts(t, "../../internal/database/types.go", "ChatStatus")
	var listed []string
	for _, s := range database.ChatStatuses {
		listed = append(listed, string(s))
	}
	if !slices.Equal(declared, listed) {
		t.Errorf("database.ChatStatuses = %q, want every declared constant in order %q", listed, declared)
	}
	js := dashboardFrozenMap(t, "../../web/public/modules/utils.js", "CHAT_STATUS_VARIANTS")
	for _, s := range declared {
		if s == "" {
			if _, ok := js[""]; ok {
				t.Error(`CHAT_STATUS_VARIANTS maps "": an empty chat_status draws no badge`)
			}
			continue
		}
		if _, ok := js[s]; !ok {
			t.Errorf("CHAT_STATUS_VARIANTS has no entry for chat_status %q", s)
		}
	}
	for k := range js {
		if !slices.Contains(declared, k) {
			t.Errorf("CHAT_STATUS_VARIANTS maps %q, which no Go code writes", k)
		}
	}
	want := map[string]string{"pending": "neutral", "downloading": "primary", "unavailable": "neutral", "finished": "success", "incomplete": "warning"}
	for k, v := range want {
		if js[k] != v {
			t.Errorf("CHAT_STATUS_VARIANTS[%q] = %q, want %q", k, js[k], v)
		}
	}
	details, err := os.ReadFile("../../web/public/modules/job-details.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(details), "chatVariantMap") || strings.Count(string(details), "chatStatusVariant(job.chatStatus)") != 2 {
		t.Error("job-details.js must read both chat badges through utils.js chatStatusVariant, not a table of its own")
	}
}

// TestBackfillStatesMatchAcrossLayers: the backfill scan states are one set in
// the monitor (which emits them), the TUI (which mirrors them, not importing
// the monitor) and the dashboard (utils.js BACKFILL_STATE), and the states
// the server's snapshot and both clients retain are the same two. Before,
// each layer spelled the four literals itself.
//
// Mutant: add a state on one side, or retain "done" — this names it.
func TestBackfillStatesMatchAcrossLayers(t *testing.T) {
	declared := declaredStringConsts(t, "../../internal/monitor/backfill_state.go", "BackfillState")
	var mon, tu []string
	for _, s := range monitor.BackfillStates {
		mon = append(mon, string(s))
	}
	for _, s := range tui.BackfillStates {
		tu = append(tu, string(s))
	}
	if !slices.Equal(mon, declared) {
		t.Errorf("monitor.BackfillStates = %q, want every declared constant %q", mon, declared)
	}
	if tuiDeclared := declaredStringConsts(t, "../../internal/tui/app.go", "BackfillState"); !slices.Equal(tu, tuiDeclared) || !slices.Equal(tu, declared) {
		t.Errorf("tui.BackfillStates = %q (declared %q), want the monitor's %q", tu, tuiDeclared, declared)
	}
	js := dashboardFrozenMap(t, "../../web/public/modules/utils.js", "BACKFILL_STATE")
	var jsVals []string
	for _, v := range js {
		jsVals = append(jsVals, v)
	}
	slices.Sort(jsVals)
	sorted := slices.Clone(declared)
	slices.Sort(sorted)
	if !slices.Equal(jsVals, sorted) {
		t.Errorf("BACKFILL_STATE values = %q, want %q", jsVals, sorted)
	}
	for _, s := range monitor.BackfillStates {
		want := s == monitor.BackfillScanning || s == monitor.BackfillError
		if s.Retained() != want {
			t.Errorf("%q.Retained() = %v, want %v (isBackfillRetained in utils.js keeps scanning and error)", s, s.Retained(), want)
		}
	}
	for _, f := range []string{"../../web/public/app.js", "../../web/public/modules/settings.js"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`state === "(scanning|error|done|idle)"`).Match(src) {
			t.Errorf("%s compares a backfill state with a literal; read BACKFILL_STATE / isBackfillRetained", f)
		}
	}
}

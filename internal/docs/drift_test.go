package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// schemaVersionClaims are the sentences that state the database's current
// schema version as a number. Each pattern must still match: a reworded
// sentence would otherwise leave this test passing over nothing.
var schemaVersionClaims = []struct {
	doc     string
	pattern *regexp.Regexp
}{
	{"data-and-storage.md", regexp.MustCompile(`Currently at v(\d+)`)},
	{"data-and-storage.md", regexp.MustCompile(`\*\*Current version: (\d+)\*\*`)},
	{"appendix-metrics.md", regexp.MustCompile(`\*\*Database schema version:\*\* (\d+)`)},
	{"SPEC.md", regexp.MustCompile(`\*\*Schema version:\*\* v(\d+)`)},
	{".claude/skills/moombox-database-migrations/SKILL.md", regexp.MustCompile(`Current schema version: \*\*v(\d+)\*\*`)},
}

// codeSchemaVersion reads the `const schemaVersion = N` declaration out of
// internal/database/migrations.go.
func codeSchemaVersion(t *testing.T, root string) int {
	t.Helper()
	file := filepath.Join(root, "internal", "database", "migrations.go")
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if name.Name != "schemaVersion" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("schemaVersion in %s is not an integer literal -- re-aim this check", file)
				}
				v, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatalf("schemaVersion %q: %v", lit.Value, err)
				}
				return v
			}
		}
	}
	t.Fatalf("no const schemaVersion in %s -- re-aim this check", file)
	return 0
}

// TestSpecDocSchemaVersionMatchesTheCode pins every sentence that states the
// current schema version to migrations.go's schemaVersion. data-and-storage.md
// said "Current version: 20" for a whole version after the bump to 21, with
// its own rules list saying "Currently at v21" above it.
//
// MUTANT: put 20 back into any of the five sentences (either one in
// data-and-storage.md, appendix-metrics.md's, SPEC.md's, the migrations
// skill's) -- that claim fails.
func TestSpecDocSchemaVersionMatchesTheCode(t *testing.T) {
	root := repoRoot(t)
	want := codeSchemaVersion(t, root)
	for _, c := range schemaVersionClaims {
		text := strings.Join(docLines(t, root, c.doc), "\n")
		ms := c.pattern.FindAllStringSubmatch(text, -1)
		if len(ms) == 0 {
			t.Errorf("%s no longer contains %q -- the sentence moved or was reworded; re-aim this check", c.doc, c.pattern)
			continue
		}
		for _, m := range ms {
			if got, _ := strconv.Atoi(m[1]); got != want {
				t.Errorf("%s says %q, but schemaVersion is %d", c.doc, m[0], want)
			}
		}
	}
}

// keyMethodsSection matches architecture.md's per-type reference headings,
// "### pkg.Type" (or several types of one package, "### pkg.A / B / C").
var keyMethodsSection = regexp.MustCompile(`^### ([a-z]+)\.([A-Z]\w*(?: / [A-Z]\w*)*)$`)

// keyMethodBullet matches one entry of a section's method list: a bullet
// whose backticked code starts with a call, "- `Name(args)...`". Callback
// fields ("- `OnStart func(...)`") are not calls and do not match.
var keyMethodBullet = regexp.MustCompile("^- `([A-Z]\\w*)\\(")

// TestSpecDocKeyMethodsExist holds architecture.md's per-type method lists to
// the code: every listed name must be a method of one of the section's types,
// or a top-level function of its package (the constructors). The database
// list named an IsInHistory that never existed; the history check is
// HasProcessed.
//
// MUTANT: rename HasProcessed back to IsInHistory in the database.Database
// list -- the check fails, naming it.
func TestSpecDocKeyMethodsExist(t *testing.T) {
	root := repoRoot(t)
	files := nonTestGoFiles(t, root)

	// declaredIn returns, for one package directory, its top-level function
	// names and its methods keyed by receiver type.
	declaredIn := func(dir string) (funcs map[string]bool, methods map[string]map[string]bool) {
		funcs, methods = map[string]bool{}, map[string]map[string]bool{}
		for _, pf := range files {
			if path.Dir(pf.rel) != dir {
				continue
			}
			for _, d := range pf.file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fd.Recv == nil || len(fd.Recv.List) != 1 {
					funcs[fd.Name.Name] = true
					continue
				}
				recv := receiverTypeName(fd.Recv.List[0].Type)
				if methods[recv] == nil {
					methods[recv] = map[string]bool{}
				}
				methods[recv][fd.Name.Name] = true
			}
		}
		return funcs, methods
	}

	checked := map[string]int{}
	var pkg string
	var types []string
	var funcs map[string]bool
	var methods map[string]map[string]bool
	for i, line := range docLines(t, root, "architecture.md") {
		if strings.HasPrefix(line, "#") {
			pkg, types = "", nil
			if m := keyMethodsSection.FindStringSubmatch(line); m != nil {
				pkg, types = m[1], strings.Split(m[2], " / ")
				funcs, methods = declaredIn("internal/" + pkg)
				if len(funcs) == 0 && len(methods) == 0 {
					t.Errorf("architecture.md:%d: %q names package %s, which internal/%s does not hold", i+1, line, pkg, pkg)
					pkg = ""
				}
			}
			continue
		}
		if pkg == "" {
			continue
		}
		m := keyMethodBullet.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		found := funcs[name]
		for _, typ := range types {
			found = found || methods[typ][name]
		}
		if !found {
			t.Errorf("architecture.md:%d lists %s under %s.%s, but no such method or %s function exists",
				i+1, name, pkg, strings.Join(types, " / "), pkg)
		}
		checked[pkg+"."+strings.Join(types, " / ")]++
	}
	// The section this check was written for must still be one it reads.
	if checked["database.Database"] == 0 {
		t.Error("no database.Database method list was checked -- the section moved or was reworded; re-aim this check")
	}
}

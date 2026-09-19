package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// specDocs are the docs whose code citations are checked: the six deep-dive
// docs under docs/spec/, plus the two hand-edited root docs added by owner
// decision O-Z (2026-09-17) after a sweep found four passages in SPEC.md
// naming symbols and wire values that do not exist, and a path in CLAUDE.md
// that does not. appendix-metrics.md (volatile numbers), design-philosophy.md
// and vision-and-purpose.md (prose) are out on purpose.
var specDocs = []string{
	"architecture.md",
	"data-and-storage.md",
	"operations.md",
	"platform-services.md",
	"security.md",
	"user-interfaces.md",
	"SPEC.md",
	"CLAUDE.md",
}

// citationPrefixes are the repo-relative roots a path citation may start
// with. A backticked token starting with anything else is prose.
var citationPrefixes = []string{"internal/", "cmd/", "web/", "tools/", "docs/", "bgutil-sidecar/", ".github/"}

// buildArtifacts are cited paths that are gitignored BUILD OUTPUTS rather than
// tracked sources: the per-platform Node blobs `go run ./tools/fetch-node`
// downloads and the tarball `bgutil-sidecar/build.mjs` produces. They are the
// exact contents of internal/bgutils/embed/.gitignore. operations.md documents
// them as a build PREREQUISITE, and a fresh clone has none of them, so
// requiring them on disk would report that prerequisite as doc rot on every
// clean checkout. The set is written out rather than asked of `git
// check-ignore` on purpose: this test starts no subprocess. A citation listed
// here is still checked -- the tracked directory that holds it must exist --
// and a .gz cited anywhere else is checked the ordinary way.
var buildArtifacts = map[string]bool{
	"internal/bgutils/embed/node-windows-amd64.gz": true,
	"internal/bgutils/embed/node-linux-amd64.gz":   true,
	"internal/bgutils/embed/node-linux-arm64.gz":   true,
	"internal/bgutils/embed/node.exe.gz":           true,
	"internal/bgutils/embed/sidecar.tar.gz":        true,
}

var (
	// fileExtRe recognises the last segment of a FILE citation.
	fileExtRe = regexp.MustCompile(`\.(go|js|mjs|ts|json|md|html|css|toml|yml|yaml|sh|txt|sql|gz|svg|ico)$`)
	// lineRefRe strips a trailing :123 or :123-456 line reference.
	lineRefRe = regexp.MustCompile(`:\d+(-\d+)?$`)
	// identRe is the shape a symbol citation must have: Name, pkg.Name,
	// Type.Method, optionally written as a call -- Name(), Name(ctx, x).
	// The docs cite functions both ways; the call suffix is stripped by
	// symbolName before the lookup.
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?(\([^()]*\))?$`)
	// callSuffixRe is that optional call suffix.
	callSuffixRe = regexp.MustCompile(`\([^()]*\)$`)
	// connectorRe is the ONLY text allowed between a symbol citation and the
	// path citation it is paired with: an optional closing bold marker, an
	// optional bracket/comma/dash, then EITHER one lowercase word followed by
	// a bracket or comma ("gate (", "helper, ") OR an optional single
	// lowercase word plus one of see/in/at/from/via ("in ", "wired at ",
	// "seeding at ", "function in "). Anything longer means the path is not
	// a citation OF that symbol -- "returns them under one `RLock`, because
	// `internal/twitch/chat_irc.go` builds one handshake" pairs nothing, and
	// neither does "`liveDownloadChat` is true (`…`)".
	connectorRe = regexp.MustCompile(`^[ \t]*(?:\*\*)?[ \t]*(?:[(\[,—-][ \t]*)?(?:[a-z]+[ \t]*[(\[,][ \t]*|(?:[a-z]+ )?(?:see|in|at|from|via) )?$`)
	// docNameRe finds the doc a "§ Heading" reference points at.
	docNameRe = regexp.MustCompile("([a-z-]+\\.md)[`)\\s]*$")
	// rfcRe excludes RFC section numbers from the heading check.
	rfcRe = regexp.MustCompile(`(?i)RFC\s+[0-9A-Za-z-]+(bis)?\s*$`)
)

// symbolName strips a call suffix: "refresh(ctx, allowFallback)" -> "refresh".
func symbolName(span string) string {
	return callSuffixRe.ReplaceAllString(span, "")
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
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

// span is one backtick-delimited run: start is the opening backtick's byte
// offset, end is one past the closing backtick.
type span struct {
	start, end int
	text       string
}

func backtickSpans(line string) []span {
	var out []span
	for i := 0; i < len(line); i++ {
		if line[i] != '`' {
			continue
		}
		j := strings.IndexByte(line[i+1:], '`')
		if j < 0 {
			break
		}
		j += i + 1
		out = append(out, span{start: i, end: j + 1, text: line[i+1 : j]})
		i = j
	}
	return out
}

// docPath resolves a specDocs entry to its file: the deep-dive docs live in
// docs/spec/, SPEC.md and CLAUDE.md at the repo root.
func docPath(root, name string) string {
	if name == "SPEC.md" || name == "CLAUDE.md" {
		return filepath.Join(root, name)
	}
	return filepath.Join(root, "docs", "spec", name)
}

// docLines reads a spec doc, LF-normalised.
func docLines(t *testing.T, root, name string) []string {
	t.Helper()
	b, err := os.ReadFile(docPath(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

// forEachProseLine calls fn for every line outside a ``` fence, 1-indexed.
// Fenced blocks are skipped because they are literal listings: package
// inventories with truncated names, shell transcripts, JSON.
func forEachProseLine(lines []string, fn func(lineNo int, line string)) {
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		fn(i+1, line)
	}
}

func hasCitationPrefix(tok string) bool {
	for _, p := range citationPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}

// parseAllowlist turns the allowlist file into a key set, plus the 1-indexed
// line numbers of entries that carry no reason. The file's header promises
// "Every entry carries its reason on the line above it", so the parser
// enforces it rather than leaving it a convention a reader has to trust: an
// entry must be immediately preceded by a `#` line. A blank line is not a
// reason, and neither is another entry. An unexplained suppression is
// indistinguishable from hidden rot, which is the one thing this file exists
// to prevent.
func parseAllowlist(s string) (map[string]bool, []int) {
	out := map[string]bool{}
	var reasonless []int
	reasonAbove := false
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			reasonAbove = false
			continue
		}
		if strings.HasPrefix(line, "#") {
			reasonAbove = true
			continue
		}
		if !reasonAbove {
			reasonless = append(reasonless, i+1)
		}
		out[line] = true
		reasonAbove = false
	}
	return out, reasonless
}

func loadAllowlist(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("citation_allowlist.txt")
	if err != nil {
		t.Fatalf("read citation_allowlist.txt: %v", err)
	}
	allow, reasonless := parseAllowlist(string(b))
	for _, lineNo := range reasonless {
		t.Errorf("citation_allowlist.txt:%d suppresses a citation with no `#` reason line above it -- the file's header promises one, and an unexplained suppression is indistinguishable from the rot it hides", lineNo)
	}
	return allow
}

func TestCitationAllowlistParsing(t *testing.T) {
	got, reasonless := parseAllowlist("# a comment\n\n# reason\narchitecture.md|internal/x/y.go\n# reason\n  operations.md|Foo|cmd/moombox/z.go  \n")
	if len(got) != 2 || !got["architecture.md|internal/x/y.go"] || !got["operations.md|Foo|cmd/moombox/z.go"] {
		t.Errorf("comments and blank lines must be dropped and entries trimmed; got %v", got)
	}
	if len(reasonless) != 0 {
		t.Errorf("reasoned entries must not be reported; got lines %v", reasonless)
	}
	// A blank line above an entry is not a reason (line 4), and neither is
	// another entry (line 5). Both must be named by their line number.
	if _, reasonless = parseAllowlist("# reason\na.md|internal/x/y.go\n\nb.md|internal/x/z.go\nc.md|internal/x/w.go\n"); len(reasonless) != 2 || reasonless[0] != 4 || reasonless[1] != 5 {
		t.Errorf("an entry with no `#` reason line above it must be reported by line number; got %v", reasonless)
	}
}

// TestRootDocsAreChecked pins owner decision O-Z: SPEC.md and CLAUDE.md, the
// two most hand-edited docs and the ones an AI reads first, are inside the
// citation checks. A sweep found four invented symbols in SPEC.md and a
// non-existent path in CLAUDE.md precisely because nothing checked them.
//
// Mutants this kills: dropping either name from specDocs (the membership
// assertion fails), and making docPath send them to docs/spec/ (docLines
// t.Fatalf's on the missing file).
func TestRootDocsAreChecked(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"SPEC.md", "CLAUDE.md"} {
		found := false
		for _, d := range specDocs {
			if d == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not in specDocs -- O-Z put both root docs under the citation checks", name)
			continue
		}
		if n := len(docLines(t, root, name)); n < 50 {
			t.Errorf("%s resolved to %d lines -- docPath is not finding the repo-root doc", name, n)
		}
	}
}

// TestCommentOnlySymbolDoesNotResolve pins the comment-exclusion rule the plan
// review set: a symbol surviving only in a `//` comment of the cited file does
// NOT satisfy a citation -- that is exactly the rot found at
// platform-services.md:907. Both rots that exercised the rule are now fixed,
// so without this nothing in the suite would notice it being relaxed.
func TestCommentOnlySymbolDoesNotResolve(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "fixture.go")
	if err := os.WriteFile(abs, []byte("package p\n\n// onlyInAComment was renamed away.\nfunc realDecl() { _ = \"inALiteral\" }\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	ff := parseGoFile(t, abs)
	if ff.resolves("onlyInAComment") {
		t.Error("a symbol mentioned only in a // comment must NOT resolve")
	}
	if !ff.resolves("realDecl") || !ff.resolves("inALiteral") {
		t.Error("a declaration and a string-literal mention must both resolve")
	}
}

// TestCitationShapes pins the two regexes the symbol check hangs on, so a
// loosening that starts pairing prose (or a tightening that stops pairing
// real citations) fails here with the offending shape named, not silently
// in the doc walk.
func TestCitationShapes(t *testing.T) {
	for _, s := range []string{"Foo", "pkg.Foo", "Type.Method", "run()", "refresh(ctx, allowFallback)", "Type.Method(origin)"} {
		if !identRe.MatchString(s) {
			t.Errorf("%q must be identifier-shaped", s)
		}
	}
	for _, s := range []string{"PUT /api/config", "0.0.0.0", "go:embed", "//go:embed", "X-Forwarded-For", "const x = false", "f(a) (b, error)"} {
		if identRe.MatchString(s) {
			t.Errorf("%q must NOT be identifier-shaped", s)
		}
	}
	if got := symbolName("CookieJar.GenerateAuthorizationHeader(origin)"); got != "CookieJar.GenerateAuthorizationHeader" {
		t.Errorf("symbolName stripped to %q", got)
	}
	for _, g := range []string{" (", ", ", " in ", " at ", " — ", " (see ", " wired at ", " seeding at ", " function in ", " gate (", " helper, ", "** (", ""} {
		if !connectorRe.MatchString(g) {
			t.Errorf("gap %q must pair", g)
		}
	}
	for _, g := range []string{", because ", " is true (", "'s no-segment backstop (both ", " | ", ". ", " (all bound in ", " reads the jar; its consumer is "} {
		if connectorRe.MatchString(g) {
			t.Errorf("gap %q must NOT pair", g)
		}
	}
}

// fileFacts is what one cited .go file's CODE contains: every identifier
// (a declaration's own name is one, so declarations are covered), every
// string literal, and -- separately -- the top-level names the file DECLARES.
// Comments are deliberately excluded -- a symbol that survives only in a
// comment is exactly the rot at platform-services.md:907.
//
// idents and declared answer two different questions. "Does this file spell
// the symbol?" keeps a citation of a struct field or a cross-file const
// alive; "is this file where the symbol comes from?" is what a reader
// following a citation actually wants, and is the only one that notices a
// citation left pointing at a CALLER after the declaration moved to a
// sibling file.
type fileFacts struct {
	idents   map[string]bool
	declared map[string]bool
	literals string
}

// resolves accepts Name, pkg.Name, Type.Method and Type.Field: the dotted
// forms fall back to their last segment, which is the identifier the file
// actually spells.
func (ff *fileFacts) resolves(sym string) bool {
	if ff.idents[sym] {
		return true
	}
	last := sym
	if i := strings.LastIndex(sym, "."); i >= 0 {
		last = sym[i+1:]
	}
	if ff.idents[last] {
		return true
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(last) + `\b`).MatchString(ff.literals)
}

// declares mirrors resolves' dotted-form fallback -- parseGoFile records a
// method as both Type.Method and Method, and pkg.Name falls back to Name --
// but consults only the declaration set. A call, an import, a struct field
// or a string literal is a mention, never a declaration.
func (ff *fileFacts) declares(sym string) bool {
	if ff.declared[sym] {
		return true
	}
	if i := strings.LastIndex(sym, "."); i >= 0 {
		return ff.declared[sym[i+1:]]
	}
	return false
}

func receiverTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	}
	return ""
}

func parseGoFile(t *testing.T, abs string) *fileFacts {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", abs, err)
	}
	ff := &fileFacts{idents: map[string]bool{}, declared: map[string]bool{}}
	var lits strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			ff.idents[n.Name] = true
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				lits.WriteString(n.Value)
				lits.WriteByte('\n')
			}
		}
		return true
	})
	ff.literals = lits.String()
	// Top-level declarations only: f.Decls, not a walk. A method is recorded
	// both as Type.Method (how the docs usually cite it) and bare (how a
	// reader may write it); grouped var/const/type specs each contribute
	// every name they bind.
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			ff.declared[d.Name.Name] = true
			if d.Recv != nil && len(d.Recv.List) == 1 {
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					ff.declared[recv+"."+d.Name.Name] = true
				}
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					ff.declared[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						ff.declared[n.Name] = true
					}
				}
			}
		}
	}
	return ff
}

// namedFacts is one sibling file of a package directory, kept with its base
// name so an error message can say where a symbol actually lives.
type namedFacts struct {
	name  string
	facts *fileFacts
}

// siblingFacts caches each package directory's non-test .go files, parsed
// once in os.ReadDir's sorted order. The doc walk cites dozens of files from
// a handful of packages; without this, a package would be re-parsed per
// citation.
var siblingFacts = map[string][]namedFacts{}

// declaredElsewhere names the first non-test .go file in dir, other than abs,
// that DECLARES sym. It is what turns "the cited file only mentions this"
// into a one-line fix: the citation's file half should name what this
// returns.
func declaredElsewhere(t *testing.T, dir, abs, sym string) (string, bool) {
	t.Helper()
	sibs, cached := siblingFacts[dir]
	if !cached {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			sibs = append(sibs, namedFacts{name: name, facts: parseGoFile(t, filepath.Join(dir, name))})
		}
		siblingFacts[dir] = sibs
	}
	self := filepath.Base(abs)
	for _, s := range sibs {
		if s.name != self && s.facts.declares(sym) {
			return s.name, true
		}
	}
	return "", false
}

// citationProblem is check (b)'s rule, in one place so the doc walk and
// TestCitationDeclaringFile exercise the same code. It returns the reason a
// symbol/file pair fails, or "" when it holds:
//
//   - the file DECLARES the symbol: the citation names its home. Fine.
//   - the file only MENTIONS it and a sibling declares it: the citation is
//     pointing at a caller (or an importer) -- a reader following it lands
//     somewhere the symbol is used, not defined. Fails, naming the sibling.
//   - the file only mentions it and nothing in the package declares it:
//     a struct field, a const or a type from another package. Fine -- a
//     mention is the best evidence available.
//   - neither: rot.
func citationProblem(t *testing.T, ff *fileFacts, abs, sym string) string {
	t.Helper()
	switch {
	case ff.declares(sym):
		return ""
	case ff.resolves(sym):
		if other, ok := declaredElsewhere(t, filepath.Dir(abs), abs, sym); ok {
			return "only mentions it -- it is declared in " + other
		}
		return ""
	default:
		return "neither declares nor mentions it (a comment does not count)"
	}
}

// TestCitationDeclaringFile pins citationProblem's three-way rule on a temp
// two-file package: a.go declares, b.go calls. Without it the difference
// between "declares" and "mentions" is invisible to the suite, and the rule
// could be relaxed back to a bare resolves() -- which is what let
// architecture.md go on citing `launchAndSupervise()` against main.go for as
// long as it did, after the function moved to launcher.go.
func TestCitationDeclaringFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("a.go", "package p\n\ntype Widget struct{ Size int }\n\nconst (\n\tLimit = 3\n\tSpare = 4\n)\n\nfunc Foo() {}\n\nfunc (w *Widget) Resize() {}\n")
	write("b.go", "package p\n\nfunc caller() {\n\tFoo()\n\tw := &Widget{Size: Limit}\n\tw.Resize()\n\t_ = \"looseEnd\"\n}\n")
	// A _test.go sibling must be invisible to declaredElsewhere: it declares
	// looseEnd, and naming it would send a reader to a file the docs never
	// cite. The "string literal" case below is what proves it is skipped.
	write("a_test.go", "package p\n\nfunc looseEnd() {}\n")

	for _, tc := range []struct {
		name, file, sym, want string
	}{
		{"declaring file passes", "a.go", "Foo", ""},
		{"caller names the declaring file", "b.go", "Foo", "only mentions it -- it is declared in a.go"},
		{"grouped const counts as a declaration", "b.go", "Limit", "only mentions it -- it is declared in a.go"},
		{"type declaration", "b.go", "Widget", "only mentions it -- it is declared in a.go"},
		{"method cited as Type.Method", "a.go", "Widget.Resize", ""},
		{"method cited from its caller", "b.go", "Widget.Resize", "only mentions it -- it is declared in a.go"},
		{"struct field: mentioned, declared nowhere at top level", "b.go", "Size", ""},
		{"string literal, declared nowhere", "b.go", "looseEnd", ""},
		{"absent everywhere is still rot", "a.go", "Ghost", "neither declares nor mentions it (a comment does not count)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			abs := filepath.Join(dir, tc.file)
			if got := citationProblem(t, parseGoFile(t, abs), abs, tc.sym); got != tc.want {
				t.Errorf("citationProblem(%s, %q) = %q, want %q", tc.file, tc.sym, got, tc.want)
			}
		})
	}
}

// TestSpecDocCitationsResolve is checks (a), (a') and (b): every path, every
// directory and every symbol the six docs cite still exists -- and, for a
// symbol, that the cited file is where it is DECLARED whenever some file in
// that package declares it. It names each failure by doc:line so the fix is
// a one-line edit, not a hunt.
//
// It also counts what it reached and fails below a floor. Without that, one
// token is enough to make this -- the largest of the three walks -- pass green
// while asserting nothing: emptying citationPrefixes recognises no citation at
// all, and dropping `go` from fileExtRe silently drops every .go path and
// every symbol pair. Both mutations survived a review battery that killed
// eleven others. The other two walks already carry this guard (the §-reference
// floor and nonTestGoFiles' file-count floor).
func TestSpecDocCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	allow := loadAllowlist(t)
	factsCache := map[string]*fileFacts{}
	files, dirs, pairs := 0, 0, 0

	for _, doc := range specDocs {
		lines := docLines(t, root, doc)
		forEachProseLine(lines, func(lineNo int, line string) {
			spans := backtickSpans(line)
			for i, sp := range spans {
				tok := sp.text
				if !hasCitationPrefix(tok) || strings.ContainsAny(tok, " \t") {
					continue
				}
				p := lineRefRe.ReplaceAllString(tok, "")

				// (a) file citation
				if fileExtRe.MatchString(p) {
					files++
					if allow[doc+"|"+p] {
						continue
					}
					if buildArtifacts[p] {
						// A gitignored build output: absent in a fresh clone,
						// so only the tracked directory that HOLDS it is
						// required to exist. Nothing here can be a .go file,
						// so there is no symbol pair to check.
						if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(path.Dir(p)))); err != nil || !st.IsDir() {
							t.Errorf("%s:%d cites `%s`, a build artifact whose directory does not exist", doc, lineNo, tok)
						}
						continue
					}
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
						t.Errorf("%s:%d cites `%s`, which does not exist", doc, lineNo, tok)
						continue
					}
					if !strings.HasSuffix(p, ".go") || i == 0 {
						continue
					}
					// (b) symbol paired with a .go citation
					prev := spans[i-1]
					if !identRe.MatchString(prev.text) || fileExtRe.MatchString(prev.text) {
						continue
					}
					if !connectorRe.MatchString(line[prev.end:sp.start]) {
						continue
					}
					pairs++
					sym := symbolName(prev.text)
					if allow[doc+"|"+sym+"|"+p] {
						continue
					}
					abs := filepath.Join(root, filepath.FromSlash(p))
					ff, ok := factsCache[abs]
					if !ok {
						ff = parseGoFile(t, abs)
						factsCache[abs] = ff
					}
					if why := citationProblem(t, ff, abs, sym); why != "" {
						t.Errorf("%s:%d cites `%s` (`%s`), but that file %s", doc, lineNo, prev.text, tok, why)
					}
					continue
				}

				// (a') directory citation: no dot in any segment
				dir := strings.TrimSuffix(p, "/")
				if strings.Contains(dir, ".") {
					continue // pkg.Type.Method and friends -- not a path
				}
				dirs++
				if allow[doc+"|"+p] {
					continue
				}
				st, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
				if err != nil || !st.IsDir() {
					t.Errorf("%s:%d cites `%s`, which is not a directory", doc, lineNo, tok)
				}
			}
		})
	}

	// The floors sit just under the counts measured when they were written --
	// 317 file citations, 59 directories, 160 symbol pairs -- with room for
	// ordinary doc editing above them. They are a vacuity guard, not a target:
	// a drop of this size means the scan stopped recognising a whole SHAPE of
	// citation, not that a paragraph was deleted. Raise them if the docs grow;
	// never lower one to make a run go green.
	if files < 250 {
		t.Errorf("only %d file citations were checked -- the scan is broken (there were 317 when this floor was written)", files)
	}
	if dirs < 40 {
		t.Errorf("only %d directory citations were checked -- the scan is broken (there were 59 when this floor was written)", dirs)
	}
	if pairs < 120 {
		t.Errorf("only %d symbol/path pairs were checked -- the scan is broken (there were 160 when this floor was written)", pairs)
	}
}

// TestSpecDocHeadingReferencesResolve is check (d). A reference resolves when
// some heading of the target doc -- backticks and a trailing parenthetical
// stripped, lowercased -- is a WORD-prefix of the text after the §. That
// handles both "§ Cookies" pointing at "## Cookies (internal/cookies/)" and
// "§ Refresh Service for the conditions" trailing off into prose. Known
// blind spot, accepted: a short heading is a word-prefix of a longer,
// non-existent one ("§ Cookies Import" would pass on "## Cookies").
func TestSpecDocHeadingReferencesResolve(t *testing.T) {
	root := repoRoot(t)
	headings := map[string][]string{}
	for _, doc := range specDocs {
		forEachProseLine(docLines(t, root, doc), func(_ int, line string) {
			if !strings.HasPrefix(line, "#") {
				return
			}
			h := strings.ReplaceAll(strings.TrimLeft(line, "# "), "`", "")
			if i := strings.LastIndex(h, " ("); i > 0 && strings.HasSuffix(strings.TrimSpace(h), ")") {
				h = h[:i]
			}
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				headings[doc] = append(headings[doc], h)
			}
		})
	}

	isWordPrefix := func(hs []string, tail string) bool {
		for _, h := range hs {
			if !strings.HasPrefix(tail, h) {
				continue
			}
			rest := tail[len(h):]
			if rest == "" {
				return true
			}
			c := rest[0]
			if !(c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return true
			}
		}
		return false
	}

	refs := 0
	for _, doc := range specDocs {
		forEachProseLine(docLines(t, root, doc), func(lineNo int, line string) {
			for idx := 0; ; {
				k := strings.Index(line[idx:], "§")
				if k < 0 {
					return
				}
				pos := idx + k
				before := line[:pos]
				after := strings.TrimLeft(line[pos+len("§"):], " ")
				idx = pos + len("§")
				if rfcRe.MatchString(before) {
					continue // RFC 6265 §4.1.2.3
				}
				if after == "" || (after[0] >= '0' && after[0] <= '9') {
					continue // spec §10
				}
				target := doc
				if m := docNameRe.FindStringSubmatch(before); m != nil {
					target = m[1]
				}
				if j := strings.IndexByte(after, '`'); j >= 0 {
					after = after[:j]
				}
				if _, known := headings[target]; !known {
					t.Logf("%s:%d references a heading in %s, which is outside the checked set", doc, lineNo, target)
					continue
				}
				refs++
				if !isWordPrefix(headings[target], strings.ToLower(after)) {
					t.Errorf("%s:%d references §%.40s in %s, which has no such heading", doc, lineNo, after, target)
				}
			}
		})
	}
	if refs < 10 {
		t.Errorf("only %d §-references were checked -- the scan is broken (there were 18 when this test was written)", refs)
	}
}

// parsedFile is one non-test Go file of the module.
type parsedFile struct {
	rel  string // slash-separated, repo-relative
	file *ast.File
}

// nonTestGoFiles parses every non-_test.go file in the module. Build tags are
// irrelevant: a Linux-only writer counts as much as a Windows one.
func nonTestGoFiles(t *testing.T, root string) []parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []parsedFile
	skip := map[string]bool{"references": true, "node_modules": true, "bgutil-sidecar": true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || skip[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, parsedFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("only %d non-test Go files found -- the walk is wrong, and every absence check below would pass vacuously", len(out))
	}
	return out
}

// absenceClaim is a doc sentence that asserts something is NOT there (or is
// pinned to one value). Each is located by key (line numbers drift), and a
// key that no longer appears is itself a failure: the sentence was reworded
// and the check must be re-aimed.
type absenceClaim struct {
	doc   string
	key   string
	why   string
	check func(t *testing.T, files []parsedFile)
}

// pilotArmed re-verifies "const livenessRecoveryArmed = true", which two docs
// quote verbatim. It was pilotDisarmed until the owner armed the pilot on
// 2026-09-03; the shape is unchanged, and the day the constant moves again both
// sentences must move with it -- this is the check that says so.
func pilotArmed(t *testing.T, files []parsedFile) {
	found := false
	for _, pf := range files {
		for _, d := range pf.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if n.Name != "livenessRecoveryArmed" {
						continue
					}
					found = true
					if i >= len(vs.Values) {
						t.Errorf("%s declares livenessRecoveryArmed without a value", pf.rel)
						continue
					}
					if id, ok := vs.Values[i].(*ast.Ident); !ok || id.Name != "true" {
						t.Errorf("%s declares livenessRecoveryArmed with a value other than the literal true -- the docs quote `const livenessRecoveryArmed = true` and must change with it", pf.rel)
					}
				}
			}
		}
	}
	if !found {
		t.Error("no production const named livenessRecoveryArmed exists -- the docs quote it; re-aim this check")
	}
}

func TestSpecDocAbsenceClaimsHold(t *testing.T) {
	root := repoRoot(t)
	files := nonTestGoFiles(t, root)

	claims := []absenceClaim{{
		doc: "data-and-storage.md",
		key: "so nothing in production feeds the interval parameter",
		why: "RefreshService's interval parameter is dead in production",
		check: func(t *testing.T, files []parsedFile) {
			calls := 0
			for _, pf := range files {
				ast.Inspect(pf.file, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok || !isCallTo(ce.Fun, "cookies", "NewRefreshService") {
						return true
					}
					calls++
					if len(ce.Args) < 2 {
						return true
					}
					lit, ok := ce.Args[1].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT || lit.Value != "0" {
						t.Errorf("%s passes a non-zero interval to NewRefreshService -- data-and-storage.md's \"nothing in production feeds the interval parameter\" is now false", pf.rel)
					}
					return true
				})
			}
			if calls == 0 {
				t.Error("no production call to cookies.NewRefreshService exists at all -- the claim's subject is gone; re-verify the sentence")
			}
		},
	}, {
		doc: "data-and-storage.md",
		key: "nothing in production ever wired it",
		why: "the Logger type's per-job buffer API was removed in 2026-07",
		check: func(t *testing.T, files []parsedFile) {
			banned := map[string]bool{"LogForJob": true, "GetJobLogs": true, "PruneJobLogs": true}
			for _, pf := range files {
				for _, d := range pf.file.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
						continue
					}
					if receiverTypeName(fd.Recv.List[0].Type) == "Logger" && banned[fd.Name.Name] {
						t.Errorf("%s declares Logger.%s -- the doc says that API was removed (the *Database methods of the same name are the live pipeline and are fine)", pf.rel, fd.Name.Name)
					}
				}
				ast.Inspect(pf.file, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "LogForJob" {
						t.Errorf("%s mentions LogForJob -- the doc says the Logger per-job buffer API is gone", pf.rel)
					}
					return true
				})
			}
		},
	}, {
		doc: "data-and-storage.md",
		key: "Nothing automatic ever prunes it",
		why: "every writer of cfg.Cookies.Platforms is known, and the only one that can shrink the list is the operator's PUT /api/config",
		check: func(t *testing.T, files []parsedFile) {
			// The three known writers, each verified by reading:
			//   cmd/moombox/services.go       -- the first-run seed (only when
			//                                    the list is empty) and the
			//                                    wizard's FinishSetup merge
			//                                    (union with the verified
			//                                    platforms: adds, never removes)
			//   internal/config/config.go     -- migrateOldFormat: copies the
			//                                    legacy [auto_cookies] list only
			//                                    when [cookies] has none
			//   internal/web/routes/config_routes.go -- PUT /api/config: the
			//                                    operator REPLACING the list,
			//                                    which is the sole removal path
			//                                    the sentence names
			want := map[string]bool{
				"cmd/moombox/services.go":              true,
				"internal/config/config.go":            true,
				"internal/web/routes/config_routes.go": true,
			}
			got := map[string]bool{}
			for _, pf := range files {
				ast.Inspect(pf.file, func(n ast.Node) bool {
					as, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range as.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Platforms" {
							continue
						}
						if outer, ok := sel.X.(*ast.SelectorExpr); ok && outer.Sel.Name == "Cookies" {
							got[pf.rel] = true
						}
					}
					return true
				})
			}
			for f := range got {
				if !want[f] {
					t.Errorf("%s assigns cfg.Cookies.Platforms and is not a known writer -- read it: if it can REMOVE a platform without an operator's request, the doc's \"Nothing automatic ever prunes it\" is false; otherwise add it to the known set with its reason", f)
				}
			}
			for f := range want {
				if !got[f] {
					t.Errorf("expected writer %s no longer assigns cfg.Cookies.Platforms -- the claim's evidence moved; re-verify it", f)
				}
			}
		},
	}, {
		doc:   "data-and-storage.md",
		key:   "const livenessRecoveryArmed = true",
		why:   "the automatic-recovery pilot is armed, and the doc quotes the constant",
		check: pilotArmed,
	}, {
		doc:   "operations.md",
		key:   "const livenessRecoveryArmed = true",
		why:   "same constant, quoted by the operations doc",
		check: pilotArmed,
	}}

	for _, c := range claims {
		body := strings.Join(docLines(t, root, c.doc), "\n")
		if !strings.Contains(body, c.key) {
			t.Errorf("%s no longer contains %q -- the claim (%s) was reworded or removed; re-aim this check", c.doc, c.key, c.why)
			continue
		}
		c.check(t, files)
	}
}

// isCallTo matches pkg.Name(...) and, inside the declaring package, Name(...).
func isCallTo(fun ast.Expr, pkg, name string) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == name
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == pkg && f.Sel.Name == name
	}
	return false
}

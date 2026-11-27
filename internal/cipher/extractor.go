package cipher

// The goja fallback's extractor: a Go port of the ejs solver that the sidecar
// runs (bgutil-sidecar/vendor/ejs/src/yt/solver, pinned by ejsPortVersion;
// owner ruling RV-12). ejs parses the player with meriyah, drops the
// statements that only have side effects, finds the URL-builder function by
// the shape of its AST, appends `_result.n` and `_result.sig`, and prints the
// program again with astring. Here the same matching runs on goja's own
// parser and AST, and the output is made by cutting and inserting source text
// at node offsets, so neither meriyah nor astring runs in the VM. The answers
// are ejs's by upstream's own test vectors (TestEjsPortVectors); the text is
// not byte-for-byte what ejs prints.
//
// Every function below names the ejs function it ports. A change to the
// vendored ejs needs the matching change here, gated by the vector test, and
// a new ejsPortVersion (TestEjsPortPinMatchesVendoredEjs).

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// ejsPortVersion is the ejs commit this port mirrors: the content of
// bgutil-sidecar/vendor/ejs/VERSION when it was last brought in line. It
// also names the prepared copies on disk (PlayerCache.FilePathPreprocessed),
// so a port of a new ejs never runs a copy the old one prepared.
const ejsPortVersion = "c47fb1b54215afa4f6a3d557ad65bb724022f6ba"

// ejsSetup is ejs's setup.ts, verbatim: the globals the player expects at
// load time, given in a bare runtime (getFromPrepared) with no DOM shim.
const ejsSetup = `
if (typeof globalThis.XMLHttpRequest === "undefined") {
    globalThis.XMLHttpRequest = { prototype: {} };
}
if (typeof URL === "undefined") {
    globalThis.location = {
        hash: "",
        host: "www.youtube.com",
        hostname: "www.youtube.com",
        href: "https://www.youtube.com/watch?v=yt-dlp-wins",
        origin: "https://www.youtube.com",
        password: "",
        pathname: "/watch",
        port: "",
        protocol: "https:",
        search: "?v=yt-dlp-wins",
        username: "",
    };
} else {
    globalThis.location = new URL("https://www.youtube.com/watch?v=yt-dlp-wins");
}
if (typeof globalThis.document === "undefined") {
    globalThis.document = Object.create(null);
}
if (typeof globalThis.navigator === "undefined") {
    globalThis.navigator = Object.create(null);
}
if (typeof globalThis.self === "undefined") {
    globalThis.self = globalThis;
}
if (typeof globalThis.window === "undefined") {
    globalThis.window = globalThis;
}
globalThis.window.location = globalThis.location;
`

// prepareStats is what preprocessPlayer found, for the compile's Debug line.
type prepareStats struct {
	kept, dropped int
	solvers       int
	routes        string // how each solver was matched: alr, pool-direct, pool-args, pool-op
	pool          bool   // extractArray found the string pool
}

// preprocessPlayer is ejs preprocessPlayer: the returned text runs as the body
// of function(_result){...} (getFromPrepared) and leaves _result.n and
// _result.sig, each a function of one challenge. A player in which no
// URL-builder function is found is an error wrapping ErrExtractorMismatch,
// where ejs would prepare a copy whose every answer throws "no solutions":
// that copy is not worth a compile or a place on disk.
func preprocessPlayer(src string) (code string, st prepareStats, err error) {
	defer func() {
		if r := recover(); r != nil {
			code, err = "", fmt.Errorf("%w: preparing the player panicked: %v", ErrExtractorMismatch, r)
		}
	}()
	prog, err := parser.ParseFile(nil, "", src, 0)
	if err != nil {
		return "", st, fmt.Errorf("%w: parse player: %v", ErrExtractorMismatch, err)
	}
	p := &ejsPort{src: src}
	block, dropFirst, err := modifyPlayerBlock(prog)
	if err != nil {
		return "", st, err
	}
	body := block.List

	// A statement's text runs from its first character, pulled back over an
	// opening parenthesis the AST does not record, to the next one's.
	starts := make([]int, len(body)+1)
	for i, s := range body {
		o := int(s.Idx0()) - 1
		for o > 0 && strings.IndexByte("( \n\r\t", src[o-1]) >= 0 {
			o--
		}
		starts[i] = o
	}
	end := int(block.RightBrace) - 1
	starts[len(body)] = end

	// modifyPlayer's filter: an expression statement stays only when it is an
	// assignment, a literal, or a call of a prototype helper.
	helpers := findPrototypeHelpers(body)
	type cut struct{ from, to int }
	var cuts []cut
	var kept []ast.Statement
	for i, s := range body {
		keep := true
		if i == 0 && dropFirst {
			keep = false
		} else if es, ok := s.(*ast.ExpressionStatement); ok {
			switch e := es.Expression.(type) {
			case *ast.CallExpression:
				name, ok := helperKey(e.Callee)
				keep = ok && helpers[name]
			case *ast.AssignExpression, *ast.StringLiteral, *ast.NumberLiteral,
				*ast.BooleanLiteral, *ast.NullLiteral, *ast.RegExpLiteral:
			default:
				keep = false
			}
		}
		if keep {
			kept = append(kept, s)
		} else {
			cuts = append(cuts, cut{starts[i], starts[i+1]})
		}
	}
	st.kept, st.dropped = len(kept), len(cuts)

	// getSolutions
	pool := extractArray(kept)
	st.pool = pool != nil
	var solvers, routes []string
	for _, s := range kept {
		if sv, route := p.extract(s, pool, kept); sv != "" {
			solvers = append(solvers, sv)
			routes = append(routes, route)
		}
	}
	st.solvers = len(solvers)
	st.routes = strings.Join(routes, ",")
	if len(solvers) == 0 {
		return "", st, fmt.Errorf("%w: no URL-builder function found (%d statements kept, string pool found: %v)",
			ErrExtractorMismatch, st.kept, st.pool)
	}

	var b strings.Builder
	b.Grow(len(src) + len(ejsSetup) + 4096)
	b.WriteString(ejsSetup)
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].from < cuts[j].from })
	at := 0
	for _, c := range cuts {
		b.WriteString(src[at:c.from])
		b.WriteString(";")
		at = c.to
	}
	b.WriteString(src[at:end])
	b.WriteString(";\n_result.n = " + multiTry(solvers, "n") + ";\n_result.sig = " + multiTry(solvers, "sig") + ";\n")
	b.WriteString(src[end:])
	return b.String(), st, nil
}

// ejsPort holds the player's source, which the output is cut from.
type ejsPort struct {
	src string
}

// text is the source of an expression, widened over any opening
// parentheses the AST leaves out (a member of a parenthesised object starts
// where the object does, inside them) until its parentheses balance.
func (p *ejsPort) text(n ast.Node) string {
	from, to := int(n.Idx0())-1, int(n.Idx1())-1
	for parenBalance(p.src[from:to]) < 0 && from > 0 && p.src[from-1] == '(' {
		from--
	}
	return p.src[from:to]
}

// parenBalance is the count of '(' less the count of ')' in s, outside
// string literals.
func parenBalance(s string) int {
	n := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(':
			n++
		case c == ')':
			n--
		}
	}
	return n
}

// modifyPlayerBlock is the block selection of ejs modifyPlayer: the body of
// the player's IIFE, in either of its two shapes. dropFirst is the second
// shape's leading `var window = this;`, which ejs splices out.
func modifyPlayerBlock(prog *ast.Program) (block *ast.BlockStatement, dropFirst bool, err error) {
	call := func(s ast.Statement) *ast.CallExpression {
		es, ok := s.(*ast.ExpressionStatement)
		if !ok {
			return nil
		}
		c, _ := es.Expression.(*ast.CallExpression)
		return c
	}
	switch len(prog.Body) {
	case 1:
		if c := call(prog.Body[0]); c != nil {
			var obj ast.Expression
			switch m := c.Callee.(type) {
			case *ast.DotExpression:
				obj = m.Left
			case *ast.BracketExpression:
				obj = m.Left
			}
			if f, ok := obj.(*ast.FunctionLiteral); ok && f.Body != nil {
				return f.Body, false, nil
			}
		}
	case 2:
		if c := call(prog.Body[1]); c != nil {
			if f, ok := c.Callee.(*ast.FunctionLiteral); ok && f.Body != nil {
				return f.Body, len(f.Body.List) > 0, nil
			}
		}
	}
	return nil, false, fmt.Errorf("%w: unexpected structure (%d top-level statements)", ErrExtractorMismatch, len(prog.Body))
}

// helperKey is the shape ejs's matchesStructure compares a prototype helper
// by: identifiers and members whose property is an identifier, the member's
// `computed` flag not looked at. ok is false for anything else.
func helperKey(e ast.Expression) (string, bool) {
	switch x := e.(type) {
	case *ast.Identifier:
		return x.Name.String(), true
	case *ast.DotExpression:
		l, ok := helperKey(x.Left)
		if !ok {
			return "", false
		}
		return l + "." + x.Identifier.Name.String(), true
	case *ast.BracketExpression:
		id, ok := x.Member.(*ast.Identifier)
		if !ok {
			return "", false
		}
		l, ok := helperKey(x.Left)
		if !ok {
			return "", false
		}
		return l + "." + id.Name.String(), true
	}
	return "", false
}

// findPrototypeHelpers is ejs findPrototypeHelpers: the targets of
// `X = function(){ ...; Y.prototype = ...; ... }` statements. A target ejs
// cannot turn into a pattern (makePrototypeHelpersIdentifier returning null)
// is left out.
func findPrototypeHelpers(body []ast.Statement) map[string]bool {
	helpers := map[string]bool{}
	for _, s := range body {
		es, ok := s.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		a, ok := es.Expression.(*ast.AssignExpression)
		if !ok {
			continue
		}
		f, ok := a.Right.(*ast.FunctionLiteral)
		if !ok || f.Body == nil {
			continue
		}
		if _, isPattern := a.Left.(ast.Pattern); isPattern {
			continue
		}
		for _, line := range f.Body.List {
			les, ok := line.(*ast.ExpressionStatement)
			if !ok {
				continue
			}
			la, ok := les.Expression.(*ast.AssignExpression)
			if !ok || !isPrototypeMember(la.Left) {
				continue
			}
			if name, ok := helperKey(a.Left); ok {
				helpers[name] = true
			}
			break
		}
	}
	return helpers
}

// isPrototypeMember: a member whose property is the identifier `prototype`.
func isPrototypeMember(e ast.Expression) bool {
	switch x := e.(type) {
	case *ast.DotExpression:
		return x.Identifier.Name.String() == "prototype"
	case *ast.BracketExpression:
		id, ok := x.Member.(*ast.Identifier)
		return ok && id.Name.String() == "prototype"
	}
	return false
}

// --- the AST walk ---

// walkNodes visits n and every node below it, parents first, children in
// field order (which is source order), as ejs's Object.values walks do. fn
// returning false stops the whole walk. A function's DeclarationList, which
// repeats bindings already in its body, is not visited.
func walkNodes(n ast.Node, fn func(ast.Node) bool) bool {
	if n == nil {
		return true
	}
	rv := reflect.ValueOf(n)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return true
	}
	if !fn(n) {
		return false
	}
	return walkStruct(rv, fn)
}

// walkEach walks each of nodes in turn, as walkNodes walks one.
func walkEach(nodes []ast.Node, fn func(ast.Node) bool) bool {
	for _, n := range nodes {
		if !walkNodes(n, fn) {
			return false
		}
	}
	return true
}

func walkStruct(rv reflect.Value, fn func(ast.Node) bool) bool {
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return true
	}
	t := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		switch t.Field(i).Name {
		case "DeclarationList", "File":
			continue
		}
		if !walkField(rv.Field(i), fn) {
			return false
		}
	}
	return true
}

func walkField(f reflect.Value, fn func(ast.Node) bool) bool {
	switch f.Kind() {
	case reflect.Interface, reflect.Ptr:
		if f.IsNil() || !f.CanInterface() {
			return true
		}
		if n, ok := f.Interface().(ast.Node); ok {
			return walkNodes(n, fn)
		}
		if f.Kind() == reflect.Ptr && f.Elem().Kind() == reflect.Struct {
			return walkStruct(f, fn) // *ParameterList and the like
		}
	case reflect.Slice:
		for i := 0; i < f.Len(); i++ {
			if !walkField(f.Index(i), fn) {
				return false
			}
		}
	case reflect.Struct:
		if f.CanAddr() && f.Addr().CanInterface() {
			if n, ok := f.Addr().Interface().(ast.Node); ok {
				return walkNodes(n, fn)
			}
		}
	}
	return true
}

// --- literals and JavaScript number semantics ---

func numLit(e ast.Expression) (float64, bool) {
	if l, ok := e.(*ast.NumberLiteral); ok {
		switch v := l.Value.(type) {
		case int64:
			return float64(v), true
		case float64:
			return v, true
		}
	}
	return 0, false
}

func strLit(e ast.Expression) (string, bool) {
	if l, ok := e.(*ast.StringLiteral); ok {
		return l.Value.String(), true
	}
	return "", false
}

// toInt32 is ECMAScript ToInt32, what the operands of ^ go through.
func toInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	m := math.Mod(math.Trunc(f), 1<<32)
	if m < 0 {
		m += 1 << 32
	}
	return int32(uint32(m))
}

// jsNumber is how JavaScript prints a number in a template string, for the
// literals ejs writes into the solver it generates.
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	// Go pads the exponent to two digits and JavaScript does not.
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[0]
	exp = strings.TrimLeft(exp[1:], "0")
	return mant + "e" + string(sign) + exp
}

func isMember(e ast.Expression) bool {
	switch e.(type) {
	case *ast.DotExpression, *ast.BracketExpression:
		return true
	}
	return false
}

func isFuncExpr(e ast.Expression) bool {
	f, ok := e.(*ast.FunctionLiteral)
	return ok && !f.Async
}

// --- nsig.ts ---

// solverOption is one candidate of ejs extract: the name the function is
// reached by, and its body.
type solverOption struct {
	name  ast.Expression
	stmts []ast.Statement
	body  []ast.Node // what hasNewWithTrue and findXorValue walk
}

// options is ejs extract's `identifier` structure match and the options it
// collects from a statement that passes it.
func options(s ast.Statement) []solverOption {
	switch st := s.(type) {
	case *ast.FunctionDeclaration:
		f := st.Function
		if f != nil && !f.Async && f.Name != nil && f.Body != nil {
			return []solverOption{{f.Name, f.Body.List, []ast.Node{f.Body}}}
		}
	case *ast.ExpressionStatement:
		a, ok := st.Expression.(*ast.AssignExpression)
		if !ok || a.Operator != token.ASSIGN || !isFuncExpr(a.Right) {
			return nil
		}
		if _, isIdent := a.Left.(*ast.Identifier); !isIdent && !isMember(a.Left) {
			return nil
		}
		if f := a.Right.(*ast.FunctionLiteral); f.Body != nil {
			return []solverOption{{a.Left, f.Body.List, []ast.Node{f.Body}}}
		}
	case *ast.VariableStatement:
		return bindingOptions(st.List)
	case *ast.LexicalDeclaration:
		return bindingOptions(st.List)
	}
	return nil
}

// bindingOptions is the VariableDeclaration branch: the declaration passes
// when any declarator is a plain function expression, and then every
// declarator named by an identifier whose initialiser has a block body and is
// not async is an option, arrows and classes included, as `fn?.body?.body`
// reads them in ejs.
func bindingOptions(list []*ast.Binding) []solverOption {
	pass := false
	for _, b := range list {
		if isFuncExpr(b.Initializer) {
			pass = true
			break
		}
	}
	if !pass {
		return nil
	}
	var out []solverOption
	for _, b := range list {
		id, ok := b.Target.(*ast.Identifier)
		if !ok {
			continue
		}
		switch f := b.Initializer.(type) {
		case *ast.FunctionLiteral:
			if !f.Async && f.Body != nil {
				out = append(out, solverOption{id, f.Body.List, []ast.Node{f.Body}})
			}
		case *ast.ArrowFunctionLiteral:
			if blk, ok := f.Body.(*ast.BlockStatement); ok && !f.Async {
				out = append(out, solverOption{id, blk.List, []ast.Node{blk}})
			}
		case *ast.ClassLiteral:
			// ejs walks the class body's elements, not the class.
			els := make([]ast.Node, len(f.Body))
			for i, el := range f.Body {
				els[i] = el
			}
			out = append(out, solverOption{id, nil, els})
		}
	}
	return out
}

// hasAlr is ejs's `asdasd` match over a function's statements: one of them is
// a call  ident.<any>("alr", "yes").
func hasAlr(stmts []ast.Statement) bool {
	for _, s := range stmts {
		es, ok := s.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		c, ok := es.Expression.(*ast.CallExpression)
		if !ok || len(c.ArgumentList) != 2 {
			continue
		}
		var obj ast.Expression
		switch m := c.Callee.(type) {
		case *ast.DotExpression:
			obj = m.Left
		case *ast.BracketExpression:
			obj = m.Left
		default:
			continue
		}
		if _, ok := obj.(*ast.Identifier); !ok {
			continue
		}
		a0, ok0 := strLit(c.ArgumentList[0])
		a1, ok1 := strLit(c.ArgumentList[1])
		if ok0 && ok1 && a0 == "alr" && a1 == "yes" {
			return true
		}
	}
	return false
}

// jsSplit is String.prototype.split with a string separator, counted in
// UTF-16 code units as JavaScript counts them.
func jsSplit(s, sep string) []string {
	if sep != "" {
		return strings.Split(s, sep)
	}
	units := utf16.Encode([]rune(s))
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = string(utf16.Decode([]uint16{u}))
	}
	return out
}

// extractArray is ejs extractArray: in the first 30 statements, a string pool
// (an array of more than 500 elements, or "...".split("...")) holding "set",
// "alr" and "yes", reduced to the three numbers the solver match compares.
func extractArray(stmts []ast.Statement) []int32 {
	if len(stmts) > 30 {
		stmts = stmts[:30]
	}
	exprs := func(s ast.Statement) []ast.Expression {
		var out []ast.Expression
		add := func(list []*ast.Binding) {
			for _, b := range list {
				if b.Initializer != nil {
					out = append(out, b.Initializer)
				}
			}
		}
		switch st := s.(type) {
		case *ast.VariableStatement:
			add(st.List)
		case *ast.LexicalDeclaration:
			add(st.List)
		case *ast.ExpressionStatement:
			if a, ok := st.Expression.(*ast.AssignExpression); ok {
				out = append(out, a.Right)
			}
		}
		return out
	}
	index := func(pool []string, s string) int {
		for i, v := range pool {
			if v == s {
				return i
			}
		}
		return -1
	}
	for _, s := range stmts {
		for _, e := range exprs(s) {
			var pool []string
			if arr, ok := e.(*ast.ArrayLiteral); ok && len(arr.Value) > 500 {
				pool = make([]string, len(arr.Value))
				for i, el := range arr.Value {
					pool[i], _ = strLit(el)
				}
			} else if c, ok := e.(*ast.CallExpression); ok && len(c.ArgumentList) == 1 {
				var obj ast.Expression
				switch m := c.Callee.(type) {
				case *ast.DotExpression:
					if m.Identifier.Name.String() == "split" {
						obj = m.Left
					}
				case *ast.BracketExpression:
					if id, ok := m.Member.(*ast.Identifier); ok && id.Name.String() == "split" {
						obj = m.Left
					}
				}
				str, ok1 := strLit(obj)
				delim, ok2 := strLit(c.ArgumentList[0])
				if obj != nil && ok1 && ok2 {
					pool = jsSplit(str, delim)
				}
			}
			if pool != nil {
				set, alr, yes := index(pool, "set"), index(pool, "alr"), index(pool, "yes")
				if set != -1 && alr != -1 && yes != -1 {
					return []int32{int32(set), int32(set ^ alr), int32(alr ^ yes)}
				}
			}
		}
	}
	return nil
}

// position is ejs getPosition's answer.
type position struct {
	val   float64
	isXor bool
}

// getPosition is ejs getPosition.
func getPosition(e ast.Expression) *position {
	if e == nil {
		return nil
	}
	if v, ok := numLit(e); ok {
		return &position{v, false}
	}
	switch x := e.(type) {
	case *ast.BracketExpression:
		return getPosition(x.Member)
	case *ast.BinaryExpression:
		if x.Operator == token.EXCLUSIVE_OR {
			if v, ok := numLit(x.Right); ok {
				return &position{v, true}
			}
			if v, ok := numLit(x.Left); ok {
				return &position{v, true}
			}
		}
	}
	return nil
}

// hasNewWithTrue is ejs hasNewWithTrue: somewhere below, `new X(a, true)` or
// `new X(a, !0)`.
func hasNewWithTrue(body []ast.Node) bool {
	has := false
	walkEach(body, func(n ast.Node) bool {
		ne, ok := n.(*ast.NewExpression)
		if !ok || len(ne.ArgumentList) < 2 {
			return true
		}
		switch a := ne.ArgumentList[1].(type) {
		case *ast.BooleanLiteral:
			has = a.Value
		case *ast.UnaryExpression:
			if v, ok := numLit(a.Operand); ok && a.Operator == token.NOT && v == 0 {
				has = true
			}
		}
		return !has
	})
	return has
}

// findXorValue is ejs findXorValue: the first call  o[k1](k2, k3, ...)  whose
// three positions reproduce the pool's numbers, and the key it implies.
func findXorValue(body []ast.Node, pool []int32) (key int32, direct, found bool) {
	walkEach(body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpression)
		if !ok || len(c.ArgumentList) < 2 {
			return true
		}
		// ejs reads callee.property, which for a non-computed member is an
		// Identifier and so has no position.
		br, ok := c.Callee.(*ast.BracketExpression)
		if !ok {
			return true
		}
		k1, k2, k3 := getPosition(br.Member), getPosition(c.ArgumentList[0]), getPosition(c.ArgumentList[1])
		if k1 == nil || k2 == nil || k3 == nil {
			return true
		}
		if toInt32(k1.val)^toInt32(k2.val) != pool[1] || toInt32(k2.val)^toInt32(k3.val) != pool[2] {
			return true
		}
		found = true
		if !k1.isXor && !k2.isXor && !k3.isXor {
			direct = true
		} else {
			key = toInt32(k1.val) ^ pool[0]
		}
		return false
	})
	return
}

// getCalleeName is ejs getCalleeName.
func getCalleeName(e ast.Expression) string {
	switch x := e.(type) {
	case *ast.Identifier:
		return x.Name.String()
	case *ast.DotExpression:
		return x.Identifier.Name.String()
	case *ast.BracketExpression:
		if id, ok := x.Member.(*ast.Identifier); ok {
			return id.Name.String()
		}
	case *ast.SequenceExpression:
		if len(x.Sequence) > 0 {
			return getCalleeName(x.Sequence[len(x.Sequence)-1])
		}
	}
	return ""
}

// findArgs is ejs findArgs: the literal arguments of the first call of name
// that produce key, by ^, + or -, or that are key themselves.
func findArgs(all []ast.Statement, name string, key int32) []string {
	var found []string
	k := float64(key)
	for _, s := range all {
		walkNodes(s, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpression)
			if !ok || len(c.ArgumentList) == 0 || getCalleeName(c.Callee) != name {
				return true
			}
			a0, ok0 := numLit(c.ArgumentList[0])
			if ok0 && len(c.ArgumentList) > 1 {
				if a1, ok1 := numLit(c.ArgumentList[1]); ok1 {
					if toInt32(a0)^toInt32(a1) == key || a0+a1 == k || a0-a1 == k {
						found = []string{jsNumber(a0), jsNumber(a1)}
						return false
					}
				}
			}
			if ok0 && a0 == k {
				found = []string{jsNumber(a0)}
				return false
			}
			return true
		})
		if found != nil {
			break
		}
	}
	return found
}

// resolveOp is ejs resolveOp: an assignment that puts key into the slot the
// statement's first initialiser reads it from.
func (p *ejsPort) resolveOp(s ast.Statement, key int32) string {
	var init ast.Expression
	first := func(list []*ast.Binding) {
		for _, b := range list {
			if b.Initializer != nil {
				init = b.Initializer
				return
			}
		}
	}
	switch st := s.(type) {
	case *ast.VariableStatement:
		first(st.List)
	case *ast.LexicalDeclaration:
		first(st.List)
	case *ast.ExpressionStatement:
		if a, ok := st.Expression.(*ast.AssignExpression); ok {
			init = a.Right
		}
	}
	if init == nil {
		return ""
	}
	k := float64(key)
	if isMember(init) {
		return p.text(init) + " = " + jsNumber(k)
	}
	b, ok := init.(*ast.BinaryExpression)
	if !ok {
		return ""
	}
	leftMember := isMember(b.Left)
	slot, lit := b.Right, b.Left
	if leftMember {
		slot, lit = b.Left, b.Right
	}
	val, okv := numLit(lit)
	if !isMember(slot) || !okv {
		return ""
	}
	var v float64
	switch b.Operator {
	case token.REMAINDER:
		if val > k {
			v = k
		} else {
			v = k + val
		}
	case token.EXCLUSIVE_OR:
		v = float64(key ^ toInt32(val))
	case token.PLUS:
		v = k - val
	case token.MINUS:
		if leftMember {
			v = k + val
		} else {
			v = val - k
		}
	case token.AND, token.OR:
		v = k
	default:
		return ""
	}
	return p.text(slot) + " = " + jsNumber(v)
}

// createSolver is ejs createSolver, with generate(expression) replaced by the
// expression's source text. The signature goes in through
// encodeURIComponent, as ejs hands it over: a raw one holding '%' throws
// "Malformed URI" in the player's own decodeURIComponent.
func (p *ejsPort) createSolver(name ast.Expression, args []string, code string) string {
	more := ""
	if len(args) > 0 {
		more = strings.Join(args, ", ") + ", "
	}
	prefix := ""
	if code != "" {
		prefix = code + ";\n  "
	}
	return `({sig, n}) => {
  ` + prefix + `const url = (` + p.text(name) + `)(` + more + `"https://youtube.com/watch?v=yt-dlp-wins", "s", sig ? encodeURIComponent(sig) : undefined);
  url.set("n", n);
  const proto = Object.getPrototypeOf(url);
  const keys = Object.keys(proto).concat(Object.getOwnPropertyNames(proto));
  for (const key of keys) {
    if (!["constructor", "set", "get", "clone"].includes(key)) {
      url[key]();
      break;
    }
  }
  const s = url.get("s");
  return {
    sig: s ? decodeURIComponent(s) : null,
    n: url.get("n") ?? null,
  };
}`
}

// extract is ejs extract: the solver a statement defines, and the route it
// was matched by, or "" when it defines none.
func (p *ejsPort) extract(s ast.Statement, pool []int32, all []ast.Statement) (solver, route string) {
	for _, o := range options(s) {
		if hasAlr(o.stmts) {
			return p.createSolver(o.name, nil, ""), "alr"
		}
		if pool == nil {
			continue
		}
		if sv, route := p.matchPoolSolver(o, pool, all); sv != "" {
			return sv, route
		}
	}
	return "", ""
}

// matchPoolSolver is ejs matchPoolSolver.
func (p *ejsPort) matchPoolSolver(o solverOption, pool []int32, all []ast.Statement) (string, string) {
	if !hasNewWithTrue(o.body) {
		return "", ""
	}
	key, direct, found := findXorValue(o.body, pool)
	if !found {
		return "", ""
	}
	if direct {
		return p.createSolver(o.name, nil, ""), "pool-direct"
	}
	name := ""
	switch x := o.name.(type) {
	case *ast.Identifier:
		name = x.Name.String()
	case *ast.DotExpression:
		name = x.Identifier.Name.String()
	case *ast.BracketExpression:
		if id, ok := x.Member.(*ast.Identifier); ok {
			name = id.Name.String()
		}
	}
	if name != "" {
		if args := findArgs(all, name, key); args != nil {
			return p.createSolver(o.name, args, ""), "pool-args"
		}
	}
	lim := o.stmts
	if len(lim) > 5 {
		lim = lim[:5]
	}
	for _, st := range lim {
		if code := p.resolveOp(st, key); code != "" {
			return p.createSolver(o.name, nil, code), "pool-op"
		}
	}
	return "", ""
}

// multiTry is ejs multiTry over makeSolver's wrappers: every solver is asked,
// and the answer stands only when they all agree.
func multiTry(solvers []string, ident string) string {
	wrapped := make([]string, len(solvers))
	for i, s := range solvers {
		wrapped[i] = "(" + ident + ") => (" + s + ")({" + ident + "})." + ident
	}
	return `(_input) => {
  const _results = new Set();
  const errors = [];
  for (const _generator of [` + strings.Join(wrapped, ",\n") + `]) {
    try {
      _results.add(_generator(_input));
    } catch (e) {
      errors.push(e);
    }
  }
  if (!_results.size) {
    throw ` + "`no solutions: ${errors.join(\", \")}`" + `;
  }
  if (_results.size !== 1) {
    throw ` + "`invalid solutions: ${[..._results].map(x => JSON.stringify(x)).join(\", \")}`" + `;
  }
  return _results.values().next().value;
}`
}

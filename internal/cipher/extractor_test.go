package cipher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja/parser"
)

// TestEjsPortPinMatchesVendoredEjs: the port names the ejs it mirrors, and
// that must be the ejs the sidecar runs. Bumping the vendored ejs without
// bringing the port in line (and checking it with TestEjsPortVectors) fails
// here.
func TestEjsPortPinMatchesVendoredEjs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "bgutil-sidecar", "vendor", "ejs", "VERSION"))
	if err != nil {
		t.Fatalf("read the vendored ejs pin: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != ejsPortVersion {
		t.Errorf("bgutil-sidecar/vendor/ejs/VERSION is %s, the Go port mirrors %s: port the ejs change (internal/cipher/extractor.go), run TestEjsPortVectors, then move ejsPortVersion",
			got, ejsPortVersion)
	}
}

// synthPlayer builds a small player in YouTube's second shape
// (`var _yt_player={};(function(g){var window=this;...})(_yt_player);`)
// around builders, the statements that define the URL-builder functions.
//
// The URL class answers like a real one: its fin method, the first
// prototype key that is not constructor/set/get/clone, reverses n and adds
// "_n", and swaps the first and last characters of the signature, which it
// reads with decodeURIComponent as players do. U takes set and get from
// Base through a prototype helper (g.inh) that only a kept call runs, and
// two side-effect statements that throw if they run are in the body: ejs
// drops them.
func synthPlayer(builders string) string {
	return synthPlayerWithPool(splitPool, builders)
}

// splitPool is the string pool real players mostly carry, a split string:
// "set" at 1, "alr" at 2, "yes" at 3, so the pool numbers extractArray
// reduces it to are [1, 1^2, 2^3] = [1, 3, 1].
const splitPool = `var P="x;set;alr;yes".split(";");`

// synthPlayerWithPool is synthPlayer with pool, the statement that defines
// the string pool P, in place of splitPool.
func synthPlayerWithPool(pool, builders string) string {
	return `var _yt_player={};(function(g){var window=this;
g.inh=function(a,b){a.prototype=Object.create(b.prototype);a.prototype.constructor=a};
var Base=function(){};
Base.prototype.set=function(k,v){this.p[k]=v;return this};
Base.prototype.get=function(k){return this.p[k]};
Base.prototype.clone=function(){return this};
var U=function(a,b){this.u=a;this.p={};this.raw=b};
g.inh(U,Base);
var nx=function(a){return a.split("").reverse().join("")+"_n"};
var sx=function(a){a=a.split("");var c=a[0];a[0]=a[a.length-1];a[a.length-1]=c;return a.join("")};
U.prototype.fin=function(){if(this.p.n!=null)this.p.n=nx(this.p.n);if(this.p.s!=null)this.p.s=encodeURIComponent(sx(decodeURIComponent(this.p.s)))};
document.title.length;
boom();
` + pool + `
` + builders + `
})(_yt_player);
`
}

const (
	// builderAlr is matched by its d.set("alr","yes") statement.
	builderAlr = `var B=function(a,b,c){var d=new U(a,!0);d.set("alr","yes");if(c)d.set(b,c);return d};`
	// builderPoolDirect sets alr through the string pool, at plain indexes.
	builderPoolDirect = `var B=function(a,b,c){var d=new U(a,!0);d[P[1]](P[2],P[3]);if(c)d.set(b,c);return d};`
	// builderPoolArgs reaches the pool through a key (4) that a call
	// elsewhere passes it.
	builderPoolArgs = `var B=function(k,a,b,c){var d=new U(a,!0);d[P[k^5]](P[k^6],P[k^7]);if(c)d.set(b,c);return d};
g.use=function(){return B(4,"https://example.com/")};`
	// builderPoolOp reads its key (13^9 = 4) from a slot nothing calls it
	// with; ejs sets the slot.
	builderPoolOp = `var B=function(a,b,c){var k=g.K^9;var d=new U(a,!0);d[P[k^5]](P[k^6],P[k^7]);if(c)d.set(b,c);return d};`
)

// poolBuilder is a builder that reads its key k (4) from what head
// computes, ahead of the pool call. params are its leading parameters.
func poolBuilder(params, head string) string {
	return `var B=function(` + params + `a,b,c){` + head + `var d=new U(a,!0);d[P[k^5]](P[k^6],P[k^7]);if(c)d.set(b,c);return d};`
}

// bigArrayPool is the pool as a literal array of more than 500 strings, the
// other form extractArray reads, with "set", "alr" and "yes" where
// splitPool has them.
func bigArrayPool() string {
	return `var P=["x","set","alr","yes"` + strings.Repeat(`,"z"`, 600) + `];`
}

// TestPreprocessPlayerRoutes: each of the ways ejs matches the URL-builder
// function prepares a player that solves n and sig, the signature with a '%'
// in it included. A signature handed to the player without ejs's
// encodeURIComponent throws "URI malformed" there. The tcl bodies (the
// string-pool routes) take branches the two committed fixture players
// (alr) never reach, and the vector bodies that do are a download CI does
// not make; each such branch has a row here. The pool-args rows pass two
// literals of which only one operator gives the key 4 (1^5, 1+3, 9-5), and
// the pool-op rows read the key through each operator resolveOp knows, set
// as ejs sets it: g.K%9 from g.K = 4 (9 > 4), g.K+2 from 2, 10-g.K from 6,
// g.K-3 from 7, g.K&7 and g.K|4 from 4, and g.K itself from 4.
//
// Mutants: createSolver not escaping the signature (the '%' rows fail);
// the prototype-helper call dropped (no fin, so nothing transforms); the
// side-effect calls kept (boom() throws); any route's match broken (no
// solver, ErrExtractorMismatch); findArgs or resolveOp computing another key
// (the pool call sets no alr, so the URL has no s and n is unchanged);
// findArgs' two-argument branch dropped, or one of its operators (the
// pool-args rows of two literals); one of resolveOp's operator cases or its
// plain-member case dropped (that pool-op row); extractArray's literal-array
// branch dropped (the array-pool row); hasNewWithTrue's `true` arm dropped
// (the new-with-true row).
func TestPreprocessPlayerRoutes(t *testing.T) {
	for _, tc := range []struct{ name, route, src string }{
		{"alr", "alr", synthPlayer(builderAlr)},
		{"pool-direct", "pool-direct", synthPlayer(builderPoolDirect)},
		{"pool-args, the key itself", "pool-args", synthPlayer(builderPoolArgs)},
		{"pool-args, x^y", "pool-args", synthPlayer(poolBuilder("x,y,", "var k=x^y;") + `g.use=function(){return B(1,5,"u")};`)},
		{"pool-args, x+y", "pool-args", synthPlayer(poolBuilder("x,y,", "var k=x+y;") + `g.use=function(){return B(1,3,"u")};`)},
		{"pool-args, x-y", "pool-args", synthPlayer(poolBuilder("x,y,", "var k=x-y;") + `g.use=function(){return B(9,5,"u")};`)},
		{"pool-op, g.K^9", "pool-op", synthPlayer(builderPoolOp)},
		{"pool-op, g.K%9", "pool-op", synthPlayer(poolBuilder("", "var k=g.K%9;"))},
		{"pool-op, g.K+2", "pool-op", synthPlayer(poolBuilder("", "var k=g.K+2;"))},
		{"pool-op, 10-g.K", "pool-op", synthPlayer(poolBuilder("", "var k=10-g.K;"))},
		{"pool-op, g.K-3", "pool-op", synthPlayer(poolBuilder("", "var k=g.K-3;"))},
		{"pool-op, g.K&7", "pool-op", synthPlayer(poolBuilder("", "var k=g.K&7;"))},
		{"pool-op, g.K|4", "pool-op", synthPlayer(poolBuilder("", "var k=g.K|4;"))},
		{"pool-op, g.K", "pool-op", synthPlayer(poolBuilder("", "var k=g.K;"))},
		{"pool as a literal array", "pool-direct", synthPlayerWithPool(bigArrayPool(), builderPoolDirect)},
		{"new with true", "pool-direct", synthPlayer(strings.Replace(builderPoolDirect, "new U(a,!0)", "new U(a,true)", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, st, err := preprocessPlayer(tc.src)
			if err != nil {
				t.Fatalf("preprocessPlayer: %v", err)
			}
			if st.routes != tc.route {
				t.Errorf("matched by %q, want %q", st.routes, tc.route)
			}
			solvers, err := getFromPrepared(code)
			if err != nil {
				t.Fatalf("getFromPrepared: %v", err)
			}
			if got, err := solvers.DecryptN("abc"); err != nil || got != "cba_n" {
				t.Errorf("n(abc) = %q, %v; want cba_n", got, err)
			}
			for in, want := range map[string]string{
				"ABCDEF":   "FBCDEA",
				"A%B=C&D%": "%%B=C&DA",
				"%2":       "2%",
			} {
				if got, err := solvers.DecryptSig(in); err != nil || got != want {
					t.Errorf("sig(%q) = %q, %v; want %q", in, got, err, want)
				}
			}
		})
	}
}

// TestPreprocessPlayerFindsNoBuilder: a player of the right shape with no
// URL-builder function, and a player of another shape, are refused with
// ErrExtractorMismatch rather than prepared into a copy that cannot answer.
func TestPreprocessPlayerFindsNoBuilder(t *testing.T) {
	for name, src := range map[string]string{
		"no builder":    synthPlayer(`var B=function(a){return a};`),
		"another shape": `var a=1;var b=2;var c=3;`,
		"unparsable":    `(function(){`,
	} {
		if _, _, err := preprocessPlayer(src); !errors.Is(err, ErrExtractorMismatch) {
			t.Errorf("%s: err = %v, want ErrExtractorMismatch", name, err)
		}
	}
}

// TestPreprocessPlayerSolversMustAgree is ejs multiTry: with two builders
// that answer differently, there is no answer, and with two that agree there
// is one.
func TestPreprocessPlayerSolversMustAgree(t *testing.T) {
	agree := builderAlr + "\n" + strings.Replace(builderPoolDirect, "var B=", "var B2=", 1)
	code, st, err := preprocessPlayer(synthPlayer(agree))
	if err != nil {
		t.Fatalf("preprocessPlayer: %v", err)
	}
	if st.solvers != 2 {
		t.Fatalf("solvers = %d, want 2", st.solvers)
	}
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if got, err := solvers.DecryptN("abc"); err != nil || got != "cba_n" {
		t.Errorf("two agreeing builders: n(abc) = %q, %v; want cba_n", got, err)
	}

	// B2 builds a URL whose n the class leaves alone.
	disagree := builderAlr + "\n" + `var B2=function(a,b,c){var d=new U(a,!0);d.set("alr","yes");d.fin=function(){};if(c)d.set(b,c);return d};`
	code, _, err = preprocessPlayer(synthPlayer(disagree))
	if err != nil {
		t.Fatalf("preprocessPlayer: %v", err)
	}
	solvers, err = getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if got, err := solvers.DecryptN("abc"); err == nil || !strings.Contains(err.Error(), "invalid solutions") {
		t.Errorf("two disagreeing builders: n(abc) = %q, %v; want an invalid-solutions error", got, err)
	}
}

// TestGetFromPreparedRunsTheSetupGlobals: the prepared copy runs in a bare
// runtime, and ejs's setup gives the player the globals it reads at load
// time: window and self are the global object, and location is YouTube's.
func TestGetFromPreparedRunsTheSetupGlobals(t *testing.T) {
	code := ejsSetup + `
_result.n = function() {
  return [window === globalThis, self === globalThis, self.location.origin, window.location.protocol, typeof document, typeof XMLHttpRequest].join(",");
};`
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	got, err := solvers.DecryptN("")
	if want := "true,true,https://www.youtube.com,https:,object,object"; err != nil || got != want {
		t.Errorf("globals = %q, %v; want %q", got, err, want)
	}
}

// TestSolverAnswerNullIsNoAnswer: ejs's solver answers null when the URL it
// built has no value for the challenge. That is an error, not the text
// "null" in a URL.
func TestSolverAnswerNullIsNoAnswer(t *testing.T) {
	solvers, err := getFromPrepared(`_result.n = function(){ return null }; _result.sig = function(){ return undefined };`)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if got, err := solvers.DecryptN("x"); err == nil {
		t.Errorf("n answered null: got %q, want an error", got)
	}
	if got, err := solvers.DecryptSig("x"); err == nil {
		t.Errorf("sig answered undefined: got %q, want an error", got)
	}
}

// TestWalkVisitsInESTreeOrder: findArgs takes the first matching call in the
// order ejs walks the AST, meriyah's ESTree key order. A for statement's test
// comes before its update there (goja's fields have Update first), and a
// do-while's body before its test (goja's have Test first). Each loop below
// holds two calls that both give the key 4 (1^5 and 2^6), so the order
// decides the args.
//
// Mutant: walkNodes without its ForStatement or DoWhileStatement case (the
// field order picks 2, 6).
func TestWalkVisitsInESTreeOrder(t *testing.T) {
	for _, src := range []string{
		"for (B(9, 9); B(1, 5); B(2, 6)) {}",
		"do { B(1, 5) } while (B(2, 6));",
	} {
		prog, err := parser.ParseFile(nil, "", src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got := findArgs(prog.Body, "B", 4); strings.Join(got, ",") != "1,5" {
			t.Errorf("findArgs over %q = %v, want [1 5]", src, got)
		}
	}
}

// TestJSNumber: the numbers the port writes into the solver are printed as
// JavaScript prints them.
func TestJSNumber(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0", -4: "-4", 13: "13", 1.5: "1.5", 1e21: "1e+21", 1e-7: "1e-7", 123456789012: "123456789012",
	} {
		if got := jsNumber(f); got != want {
			t.Errorf("jsNumber(%v) = %q, want %q", f, got, want)
		}
	}
}

// TestToInt32 is ECMAScript ToInt32 on the values ^ sees.
func TestToInt32(t *testing.T) {
	for f, want := range map[float64]int32{
		5: 5, -1: -1, 4294967296: 0, 4294967297: 1, 2147483648: -2147483648, 3.9: 3, -3.9: -3,
	} {
		if got := toInt32(f); got != want {
			t.Errorf("toInt32(%v) = %d, want %d", f, got, want)
		}
	}
}

func TestCacheKey(t *testing.T) {
	key1 := CacheKey("https://www.youtube.com/s/player/abc123/base.js")
	key2 := CacheKey("https://www.youtube.com/s/player/abc123/base.js")
	key3 := CacheKey("https://www.youtube.com/s/player/def456/base.js")

	if key1 != key2 {
		t.Error("same URL should produce same cache key")
	}
	if key1 == key3 {
		t.Error("different URLs should produce different cache keys")
	}
}

func TestPlayerIDFromURL(t *testing.T) {
	id := PlayerIDFromURL("https://www.youtube.com/s/player/abc123/player_ias.vflset/en_US/base.js")
	if id != "abc123" {
		t.Errorf("expected abc123, got %q", id)
	}
}

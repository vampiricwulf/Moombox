package cipher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mbgoja "github.com/vampiricwulf/Moombox/internal/goja"
)

func TestExtractFunctionByName(t *testing.T) {
	js := `var abc=function(a){a=a.split("");helper.func1(a,3);helper.func2(a);return a.join("")};`

	body, err := extractFunctionByName(js, "abc")
	if err != nil {
		t.Fatalf("extractFunctionByName: %v", err)
	}
	if !strings.Contains(body, "a.split") {
		t.Errorf("expected body to contain a.split, got %q", body)
	}
	if !strings.Contains(body, "a.join") {
		t.Errorf("expected body to contain a.join, got %q", body)
	}
}

func TestExtractFunctionByName_FuncDecl(t *testing.T) {
	js := `function myFunc(x){return x*2;}`

	body, err := extractFunctionByName(js, "myFunc")
	if err != nil {
		t.Fatalf("extractFunctionByName: %v", err)
	}
	if !strings.Contains(body, "return x*2") {
		t.Errorf("expected body to contain return x*2, got %q", body)
	}
}

func TestExtractObjectByName(t *testing.T) {
	js := `var helper={swap:function(a,b){var c=a[0];a[0]=a[b%a.length];a[b%a.length]=c},reverse:function(a){a.reverse()},splice:function(a,b){a.splice(0,b)}};`

	body, err := extractObjectByName(js, "helper")
	if err != nil {
		t.Fatalf("extractObjectByName: %v", err)
	}
	if !strings.Contains(body, "swap") {
		t.Errorf("expected body to contain swap, got %q", body)
	}
	if !strings.Contains(body, "reverse") {
		t.Errorf("expected body to contain reverse, got %q", body)
	}
}

func TestExtractSigFunction(t *testing.T) {
	// Simulated player JS with sig function
	js := `var Xo={wS:function(a,b){var c=a[0];a[0]=a[b%a.length];a[b%a.length]=c},Nv:function(a){a.reverse()},QF:function(a,b){a.splice(0,b)}};var $N=function(a){a=a.split("");Xo.QF(a,1);Xo.wS(a,56);Xo.Nv(a);Xo.QF(a,1);Xo.wS(a,10);Xo.QF(a,2);return a.join("")};c&&d.set(e, encodeURIComponent($N(f)));`

	code, err := extractSigFunction(js)
	if err != nil {
		t.Fatalf("extractSigFunction: %v", err)
	}
	if !strings.Contains(code, "Xo") {
		t.Error("expected code to contain helper object Xo")
	}
	if !strings.Contains(code, "$N") {
		t.Error("expected code to contain function $N")
	}
	if !strings.Contains(code, "_decipher") {
		t.Error("expected code to contain _decipher wrapper")
	}
}

func TestExtractNFunction(t *testing.T) {
	// Simulated player JS with n-parameter function
	js := `var nFunc=function(a){var b=a.split(""),c=[function(d){return d.reverse()},function(d){return d.slice(1)}];c[0](b);c[1](b);return b.join("")};b=a.get("n");if(b){b=nFunc(b);a.set("n",b)};`

	// Add a pattern that our extractor can find
	js2 := `.get("n"))&&(b=nFunc(b),a.set("n",b));` + js

	code, err := extractNFunction(js2)
	if err != nil {
		t.Fatalf("extractNFunction: %v", err)
	}
	if !strings.Contains(code, "nFunc") {
		t.Error("expected code to contain nFunc")
	}
	if !strings.Contains(code, "_nTransform") {
		t.Error("expected code to contain _nTransform wrapper")
	}
}

func TestResolveArrayFunction(t *testing.T) {
	js := `var myArr=[funcA,funcB,funcC];`

	name, err := resolveArrayFunction(js, "myArr", "1")
	if err != nil {
		t.Fatalf("resolveArrayFunction: %v", err)
	}
	if name != "funcB" {
		t.Errorf("expected funcB, got %q", name)
	}
}

func TestPreprocessPlayer(t *testing.T) {
	// Minimal mock player JS that has both sig and n patterns
	playerJS := `
c&&d.set(e, encodeURIComponent(sigFunc(f)));
var helperObj={sw:function(a,b){var c=a[0];a[0]=a[b%a.length];a[b%a.length]=c},rv:function(a){a.reverse()},sp:function(a,b){a.splice(0,b)}};
var sigFunc=function(a){a=a.split("");helperObj.sp(a,2);helperObj.sw(a,5);helperObj.rv(a);return a.join("")};
.get("n"))&&(b=nTransFunc(b),a.set("n",b));
var nTransFunc=function(a){var b=a.split("");b.reverse();return b.join("")};
`

	code, err := preprocessPlayer(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayer: %v", err)
	}

	// Should contain result assignments. As of test.34 (cipher.md D1/D4),
	// browser stubs are provided by goja.NewRuntimeForCipher inside
	// getFromPrepared rather than prepended setupCode, so the absence of
	// a "XMLHttpRequest" string here is expected.
	if !strings.Contains(code, "_result.sig") {
		t.Error("expected _result.sig assignment")
	}
	if !strings.Contains(code, "_result.n") {
		t.Error("expected _result.n assignment")
	}
}

func TestFindNArrayCandidates(t *testing.T) {
	// Simulates the pattern found in current YouTube player.js (main variant)
	js := `;var eiz;g.sD=class{};eiz=[jVV];g.P2=class{};`
	names := findNArrayCandidates(js)
	if len(names) != 1 {
		t.Fatalf("expected 1 n-param candidate, got %d: %v", len(names), names)
	}
	if names[0] != "jVV" {
		t.Errorf("expected jVV, got %q", names[0])
	}
}

func TestFindNArrayCandidates_TVVariant(t *testing.T) {
	// TV/ES6 variant uses bare assignments without leading semicolons,
	// and may use "var X=[func];" declarations
	tests := []struct {
		name     string
		js       string
		expected string
	}{
		{"var declaration", "var eiz=[jVV];\nother code", "jVV"},
		{"bare assignment on newline", "some code\neiz=[jVV];\nother", "jVV"},
		{"after semicolon", ";eiz=[jVV];", "jVV"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names := findNArrayCandidates(tt.js)
			if len(names) == 0 {
				t.Fatalf("expected at least 1 n-param candidate, got 0")
			}
			if names[0] != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, names[0])
			}
		})
	}
}

func TestFindSigCandidates(t *testing.T) {
	// Simulates the pattern found in current YouTube player.js
	js := `Ct=function(k,U,n){k=new g.sD(k,!0);k.set("alr","yes");n&&(n=fw(26,decodeURIComponent(n)),k.set(U,encodeURIComponent(n)));return k};`
	candidates := findSigCandidates(js)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 sig candidate, got %d: %v", len(candidates), candidates)
	}
	if candidates[0].funcName != "fw" {
		t.Errorf("expected fw, got %q", candidates[0].funcName)
	}
	if candidates[0].literal != "26" {
		t.Errorf("expected literal 26, got %q", candidates[0].literal)
	}
}

func TestFindURLClassName(t *testing.T) {
	tests := []struct {
		name     string
		js       string
		expected string
	}{
		{"current g.XX", `(new g.sB(Z,!0)).get("n")`, "g.sB"},
		{"different namespace", `(new h.Foo(Z,!0)).get("n")`, "h.Foo"},
		{"underscore namespace", `(new _yt.xY(Z,!0)).get("n")`, "_yt.xY"},
		{"with true keyword", `(new g.sB(Z,true)).get("n")`, "g.sB"},
		{"bare identifier", `(new UrlBuilder(Z,!0)).get("n")`, "UrlBuilder"},
		{"not found", `var x = 42;`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findURLClassName(tt.js)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestFindAlrTransformChains(t *testing.T) {
	// Multiple ALR markers — only the last one has the transform pattern nearby.
	// The first ALR has a decoy N&&(N= pattern, but it's 250+ chars away (outside
	// the 200-char proximity window). The second ALR has no transform pattern nearby.
	// The third ALR is the correct sig function with the transform within 15 chars.
	js := `g.ba=function(Z,k,N){` +
		`var a=new dv(k);a.get("alr")||a.set("alr","yes");` +
		strings.Repeat("x", 250) + // spacer pushes decoy outside 200-char proximity
		`N&&(N=Z.U,a=k.U);return a};` +
		strings.Repeat("x", 300) + // spacer
		`this.rV.set("alr","yes");w3R(this.A1,N,Z);` + // no transform pattern nearby
		strings.Repeat("x", 300) + // spacer
		`n8=function(Z,k,N){k=k===void 0?"":k;N=N===void 0?"":N;` +
		`Z=new g.sB(Z,!0);Z.set("alr","yes");` +
		`N&&(N=JI(49,1919,f1(33,6560,N)),Z.set(k,UE(65,3973,N)));return Z};`

	chains := findAlrTransformChains(js)
	if len(chains) == 0 {
		t.Fatal("expected at least one ALR transform chain")
	}

	// Should find JI(49,1919,f1(33,6560,sig)) from the last ALR marker
	found := false
	for _, chain := range chains {
		if strings.Contains(chain, "JI(49,1919,f1(33,6560,sig))") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected chain containing JI(49,1919,f1(33,6560,sig)), got %v", chains)
	}

	// Should NOT contain garbage like "Z.U" from the decoy (it's outside proximity)
	for _, chain := range chains {
		if chain == "Z.U" {
			t.Errorf("false match: got garbage chain %q from decoy ALR marker", chain)
		}
	}

	// Exactly one chain should be found (the correct one)
	if len(chains) != 1 {
		t.Errorf("expected exactly 1 chain, got %d: %v", len(chains), chains)
	}
}

func TestFindAlrTransformChains_NotFound(t *testing.T) {
	js := `var foo=function(a){return a+1};`
	chains := findAlrTransformChains(js)
	if len(chains) != 0 {
		t.Errorf("expected no chains, got %v", chains)
	}
}

func TestFindAlrTransformChains_SingleQuote(t *testing.T) {
	// Single-quote variant of the ALR marker
	js := `eg=function(r,p,I){r=new g.pQ(r,!0);r.set('alr','yes');` +
		`I&&(I=fQ(84,4692,fQ(16,4852,I)),r.set(p,RD(10,4164,I)));return r};`
	chains := findAlrTransformChains(js)
	if len(chains) == 0 {
		t.Fatal("expected chain from single-quote ALR marker")
	}
	if !strings.Contains(chains[0], "fQ(84,4692,fQ(16,4852,sig))") {
		t.Errorf("expected fQ(84,4692,fQ(16,4852,sig)), got %q", chains[0])
	}
}

func TestPreprocessPlayerFull(t *testing.T) {
	// Minimal mock that simulates the IIFE structure of YouTube player.js
	playerJS := `var _yt_player={};(function(g){var window=this;'use strict';var A="split;join;reverse".split(";");var TZ={Zm:function(k,U){var n=k[0];k[0]=k[U%k.length];k[U%k.length]=n},EF:function(k){k.reverse()},Pi:function(k,U){k.splice(0,U)}};var fw=function(k,U){if((k>>1&3)==1){var w=U.split("");TZ.Zm(w,3);TZ.EF(w);TZ.Pi(w,1);return w.join("")}return U};var Ct=function(k,U,n){n&&(n=fw(26,decodeURIComponent(n)));return n};var jVV=function(k){return k+"_n_transformed"};var eiz;eiz=[jVV];})(_yt_player);`

	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}

	// Should contain setup code
	if !strings.Contains(code, "_multiTry") {
		t.Error("expected _multiTry helper in output")
	}

	// Should contain the full player code
	if !strings.Contains(code, "_yt_player") {
		t.Error("expected _yt_player in output")
	}

	// Should NOT contain "var window=this;"
	if strings.Contains(code, "var window=this;") {
		t.Error("var window=this; should have been stripped")
	}

	// Should contain solver bindings
	if !strings.Contains(code, "_result.n") {
		t.Error("expected _result.n binding")
	}
	if !strings.Contains(code, "_result.sig") {
		t.Error("expected _result.sig binding")
	}

	// Try executing the preprocessed code
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if solvers.N == nil {
		t.Fatal("expected N solver")
	}
	if solvers.Sig == nil {
		t.Fatal("expected Sig solver")
	}

	// Test N function
	nResult, err := solvers.N("test123")
	if err != nil {
		t.Fatalf("N solver: %v", err)
	}
	if nResult != "test123_n_transformed" {
		t.Errorf("N: expected 'test123_n_transformed', got %q", nResult)
	}
}

func TestPreprocessPlayerFull_WindowStripping(t *testing.T) {
	// Whitespace variation in "var window=this;"
	playerJS := `var _yt_player={};(function(g){ var  window = this ;'use strict';var jVV=function(k){return k+"_done"};var eiz;eiz=[jVV];})(_yt_player);`
	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}
	if strings.Contains(code, "var  window = this ;") {
		t.Error("whitespace-variant 'var window=this;' should have been stripped")
	}
}

func TestMultiTryRejectsIdentity(t *testing.T) {
	// _multiTry should reject candidates that return the input unchanged
	code := fullPlayerSetupCode + `
_result.sig = _multiTry([
  function(x) { return x; },
  function(x) { return x + "_transformed"; }
]);
`
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if solvers.Sig == nil {
		t.Fatal("expected non-nil sig solver")
	}
	result, err := solvers.Sig("testinput")
	if err != nil {
		t.Fatalf("sig solver: %v", err)
	}
	if result == "testinput" {
		t.Error("_multiTry should reject identity function, but got input back unchanged")
	}
	if result != "testinput_transformed" {
		t.Errorf("expected 'testinput_transformed', got %q", result)
	}
}

func TestPreprocessPlayerFullAlrSig(t *testing.T) {
	// Tests the newer YouTube player pattern where the sig decipher is in
	// the URL builder function identified by set("alr","yes").
	// No standalone n-param function exists — N solver should be nil.
	playerJS := `var _yt_player={};(function(g){` +
		// OF is the sig decipher dispatcher
		`var OF=function(k,m,v){if(k===24){var w=v.split("");w.reverse();w.splice(0,2);return w.join("")}return v};` +
		// XF is encode/decode dispatcher
		`var XF=function(k,m,v){if(k===82){return decodeURIComponent(v)}if(k===2){return encodeURIComponent(v)}return v};` +
		// eg is the URL builder with set("alr","yes") marker
		`var eg=function(r,p,I){r={};r.set=function(a,b){};r.set("alr","yes");I&&(I=OF(24,6183,XF(82,6137,I)),r.set(p,XF(2,8431,I)));return r};` +
		`})(_yt_player);`

	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}

	// Should contain sig binding from ALR transform chain
	if !strings.Contains(code, "_result.sig") {
		t.Error("expected _result.sig binding")
	}

	// Execute and test
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	// N should be nil (no standalone n-param function in this player)
	if solvers.N != nil {
		t.Error("expected N solver to be nil for new player format")
	}

	// Sig should work — OF(24,6183,...) reverses and splices
	if solvers.Sig == nil {
		t.Fatal("expected Sig solver")
	}
	// OF(24,6183,sig): reverse + splice(0,2)
	// Input "ABCDEF" → reverse "FEDCBA" → splice(0,2) "DCBA"
	sigResult, err := solvers.Sig("ABCDEF")
	if err != nil {
		t.Fatalf("Sig solver: %v", err)
	}
	if sigResult != "DCBA" {
		t.Errorf("Sig: expected 'DCBA', got %q", sigResult)
	}
}

func TestPreprocessPlayerFullRealPlayer(t *testing.T) {
	// Test with real cached player.js if available
	// This test is skipped in CI (no cache), but validates the full pipeline locally
	cacheDir := ""
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	cacheDir = filepath.Join(home, ".cache", "yt-cipher", "player_cache")

	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) == 0 {
		t.Skip("no cached player.js files found")
	}

	// Use the most recent .js file
	var newest os.DirEntry
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			if newest == nil {
				newest = e
			} else {
				ni, _ := newest.Info()
				ei, _ := e.Info()
				if ei != nil && ni != nil && ei.ModTime().After(ni.ModTime()) {
					newest = e
				}
			}
		}
	}
	if newest == nil {
		t.Skip("no .js files in player cache")
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, newest.Name()))
	if err != nil {
		t.Fatalf("read player.js: %v", err)
	}

	playerJS := string(data)
	t.Logf("Testing with player.js: %s (%d bytes)", newest.Name(), len(data))

	// Log candidate finding results (informational — newer players may not
	// have old-style candidates, relying on ALR chain + URL builder instead)
	nFuncs := findNArrayCandidates(playerJS)
	t.Logf("N-param array candidates: %d %v", len(nFuncs), nFuncs)

	sigCands := findSigCandidates(playerJS)
	t.Logf("Sig old candidates: %d %v", len(sigCands), sigCands)

	alrChains := findAlrTransformChains(playerJS)
	t.Logf("ALR sig chains found: %d", len(alrChains))

	// At least one sig strategy must work
	if len(sigCands) == 0 && len(alrChains) == 0 {
		t.Error("expected at least one sig strategy (old candidates or ALR chain)")
	}

	// Test full preprocessing
	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}
	t.Logf("Preprocessed code length: %d bytes", len(code))

	// Test execution in Goja
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	if solvers.Sig != nil {
		t.Log("Sig solver: OK")
		// Test with a dummy signature (won't produce meaningful output but shouldn't crash)
		result, err := solvers.Sig("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
		if err != nil {
			t.Logf("Sig solver call (expected to work): %v", err)
		} else {
			t.Logf("Sig result: %q", result)
		}
	} else {
		t.Error("Sig solver is nil")
	}

	if solvers.N != nil {
		t.Log("N solver: OK")
		result, err := solvers.N("abc123def456")
		if err != nil {
			t.Logf("N solver call (may fail with dummy input): %v", err)
		} else {
			t.Logf("N result: %q", result)
		}
	} else {
		t.Log("N solver is nil (may be expected for newer players using URL builder)")
	}
}

func TestPreprocessPlayer74edf1a3(t *testing.T) {
	data, err := os.ReadFile("testdata/player_74edf1a3.js")
	if err != nil {
		t.Skip("player_74edf1a3.js not available")
	}

	playerJS := string(data)
	t.Logf("Player size: %d bytes", len(data))

	// Check candidate finding
	nFuncs := findNArrayCandidates(playerJS)
	t.Logf("N-param array candidates: %d %v", len(nFuncs), nFuncs)

	sigCands := findSigCandidates(playerJS)
	t.Logf("Sig old candidates: %d %v", len(sigCands), sigCands)

	alrChains := findAlrTransformChains(playerJS)
	t.Logf("ALR sig chains found: %d", len(alrChains))
	for i, chain := range alrChains {
		t.Logf("  ALR chain %d (len=%d): %s", i, len(chain), chain)
	}

	// The ALR chain must be substantive (not garbage like "Z.U")
	if len(alrChains) > 0 {
		for _, chain := range alrChains {
			if len(chain) < 10 {
				t.Errorf("ALR chain too short (likely false match): %q", chain)
			}
		}
	}

	// URL class detection
	urlClass := findURLClassName(playerJS)
	t.Logf("URL class: %q", urlClass)
	if urlClass == "" {
		t.Error("expected URL class to be detected (e.g., g.sB)")
	}

	// Full preprocessing
	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}
	t.Logf("Preprocessed code: %d bytes", len(code))

	// Execute in Goja
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	// N solver must work and transform the input
	if solvers.N == nil {
		t.Fatal("N solver is nil — this causes 403 on all DASH segments")
	}
	nResult, err := solvers.N("abc123def456")
	if err != nil {
		t.Fatalf("N solver error: %v", err)
	}
	t.Logf("N result: %q (input was abc123def456)", nResult)
	if nResult == "abc123def456" {
		t.Error("N solver returned input unchanged — not actually transforming")
	}
	if nResult == "" {
		t.Error("N solver returned empty string")
	}

	// Sig solver must work and transform the input
	if solvers.Sig == nil {
		t.Fatal("Sig solver is nil — this causes 403 on streams requiring signature decryption")
	}
	sigInput := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	sigResult, err := solvers.Sig(sigInput)
	if err != nil {
		t.Fatalf("Sig solver error: %v", err)
	}
	t.Logf("Sig result: %q (input len=%d, output len=%d)", sigResult, len(sigInput), len(sigResult))
	if sigResult == sigInput {
		t.Error("Sig solver returned input unchanged — not actually transforming")
	}
	if sigResult == "" {
		t.Error("Sig solver returned empty string")
	}
}

func TestSetupCodeGlobalUnification(t *testing.T) {
	// Verifies that window, self, and globalThis all reference the same object
	// and that self.location.origin works — the scenario that motivated ejs commit
	// 68448fa ("Expose window global as self").
	//
	// YouTube players (e.g., player_74edf1a3.js line 1201) access
	// self.location.protocol, so these globals must be unified.
	//
	// As of test.34 the unification is done by goja.NewRuntimeForCipher (audit
	// cipher.md D1/D4); legacy setupCode + fullPlayerSetupCode no longer carry
	// these stubs. The test exercises the constructor directly so future drift
	// surfaces here rather than during a real cipher compile.

	vm, err := mbgoja.NewRuntimeForCipher("")
	if err != nil {
		t.Fatalf("NewRuntimeForCipher: %v", err)
	}

	// window === globalThis
	v, err := vm.RunString("window === globalThis")
	if err != nil {
		t.Fatalf("window === globalThis: %v", err)
	}
	if !v.ToBoolean() {
		t.Error("window !== globalThis — must be the same object")
	}

	// self === globalThis
	v, err = vm.RunString("self === globalThis")
	if err != nil {
		t.Fatalf("self === globalThis: %v", err)
	}
	if !v.ToBoolean() {
		t.Error("self !== globalThis — must be the same object")
	}

	// window === self (transitive, but verify explicitly)
	v, err = vm.RunString("window === self")
	if err != nil {
		t.Fatalf("window === self: %v", err)
	}
	if !v.ToBoolean() {
		t.Error("window !== self — must be the same object")
	}

	// self.location.origin — the access pattern YouTube players use
	v, err = vm.RunString("self.location.origin")
	if err != nil {
		t.Fatalf("self.location.origin: %v", err)
	}
	if v.String() != "https://www.youtube.com" {
		t.Errorf("self.location.origin = %q, want %q", v.String(), "https://www.youtube.com")
	}

	// window.location.origin
	v, err = vm.RunString("window.location.origin")
	if err != nil {
		t.Fatalf("window.location.origin: %v", err)
	}
	if v.String() != "https://www.youtube.com" {
		t.Errorf("window.location.origin = %q, want %q", v.String(), "https://www.youtube.com")
	}

	// self.location.protocol — accessed by real player code
	v, err = vm.RunString("self.location.protocol")
	if err != nil {
		t.Fatalf("self.location.protocol: %v", err)
	}
	if v.String() != "https:" {
		t.Errorf("self.location.protocol = %q, want %q", v.String(), "https:")
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

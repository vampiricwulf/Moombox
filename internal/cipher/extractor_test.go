package cipher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// Should contain setup code
	if !strings.Contains(code, "XMLHttpRequest") {
		t.Error("expected setup code with XMLHttpRequest")
	}

	// Should contain result assignments
	if !strings.Contains(code, "_result.sig") {
		t.Error("expected _result.sig assignment")
	}
	if !strings.Contains(code, "_result.n") {
		t.Error("expected _result.n assignment")
	}
}

func TestFindNArrayCandidates(t *testing.T) {
	// Simulates the pattern found in current YouTube player.js
	js := `;var eiz;g.sD=class{};eiz=[jVV];g.P2=class{};`
	names := findNArrayCandidates(js)
	if len(names) != 1 {
		t.Fatalf("expected 1 n-param candidate, got %d: %v", len(names), names)
	}
	if names[0] != "jVV" {
		t.Errorf("expected jVV, got %q", names[0])
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

	// Test candidate finding
	nFuncs := findNArrayCandidates(playerJS)
	t.Logf("N-param candidates: %v", nFuncs)
	if len(nFuncs) == 0 {
		t.Error("expected at least one n-param candidate")
	}

	sigCands := findSigCandidates(playerJS)
	t.Logf("Sig candidates: %v", sigCands)
	if len(sigCands) == 0 {
		t.Error("expected at least one sig candidate")
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
		t.Error("N solver is nil")
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

package cipher

import (
	"fmt"
	"regexp"
	"strings"
)

// --- Full player approach (matches TypeScript AST-based method) ---
// Instead of extracting individual functions with regex (fragile),
// we include the ENTIRE player.js with solver bindings inserted.
// This mirrors the TS approach: parse full player -> find candidates -> append bindings.

// findNArrayCandidates finds n-parameter function candidates by searching for
// single-element array assignments: varName=[funcName];
// This matches the TypeScript AST pattern: ArrayExpression with 1 Identifier element.
// Two patterns are needed: the main variant uses ";X=[func];" while the tv/ES6
// variant uses bare "X=[func];" without a leading semicolon.
var nArrayPatterns = []*regexp.Regexp{
	// Main variant: ;varName=[funcName];
	regexp.MustCompile(`;([a-zA-Z_$][\w$]*)\s*=\s*\[([a-zA-Z_$][\w$]*)\]\s*;`),
	// TV/ES6 variant: var varName=[funcName]; or varName=[funcName]; (no leading ;)
	regexp.MustCompile(`(?:^|[;\n])\s*(?:var\s+)?([a-zA-Z_$][\w$]*)\s*=\s*\[([a-zA-Z_$][\w$]*)\]\s*;`),
}

func findNArrayCandidates(playerJS string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, pattern := range nArrayPatterns {
		matches := pattern.FindAllStringSubmatch(playerJS, -1)
		for _, m := range matches {
			funcName := m[2]
			if !seen[funcName] {
				seen[funcName] = true
				names = append(names, funcName)
			}
		}
	}
	return names
}

// findSigCandidates looks for patterns like:
//
//	identifier && (identifier = funcName(literal, decodeURIComponent(identifier)), ...)
//
// This matches the TypeScript AST pattern: LogicalExpression with SequenceExpression
// containing AssignmentExpression with CallExpression(Identifier, decodeURIComponent(...)).
var sigCandidatePattern = regexp.MustCompile(
	`\w+&&\(\w+=([a-zA-Z_$][\w$]*)\((\d+),decodeURIComponent\(\w+\)\)`,
)

func findSigCandidates(playerJS string) []sigCandidate {
	matches := sigCandidatePattern.FindAllStringSubmatch(playerJS, -1)
	var candidates []sigCandidate
	seen := make(map[string]bool)
	for _, m := range matches {
		key := m[1] + ":" + m[2]
		if !seen[key] {
			seen[key] = true
			candidates = append(candidates, sigCandidate{funcName: m[1], literal: m[2]})
		}
	}
	return candidates
}

// alrTransformHeadPattern matches the start of a transform chain after set("alr","yes").
// Captures the parameter name and the start of the assignment.
var alrTransformHeadPattern = regexp.MustCompile(`(\w+)&&\((\w+)=`)

// findAlrTransformChain finds the signature decipher chain in YouTube's newer
// player.js format. The URL builder function is identified by its set("alr","yes")
// marker. The transform chain after it applies signature decryption:
//
//	eg=function(r,p="",I=""){r=new g.fb(r,!0);r.set("alr","yes");
//	  I&&(I=OF(24,6183,XF(82,6137,I)),r[O[8]](p,XF(2,8431,I)));return D};
//
// The chain is: OF(24,6183,XF(82,6137,I)) = decipher(decodeURIComponent(sig)).
// Returns the expression with the parameter replaced by "sig", or empty string if not found.
func findAlrTransformChain(playerJS string) string {
	// Find the URL builder function by its unique marker: set("alr","yes")
	alrIdx := strings.Index(playerJS, `set("alr","yes");`)
	if alrIdx < 0 {
		return ""
	}

	rest := playerJS[alrIdx+len(`set("alr","yes");`):]

	// Match: PARAM&&(PARAM=
	m := alrTransformHeadPattern.FindStringSubmatchIndex(rest)
	if m == nil {
		return ""
	}

	param := rest[m[2]:m[3]]
	assignParam := rest[m[4]:m[5]]
	if param != assignParam {
		return ""
	}

	// Extract the transform expression by tracking parenthesis depth.
	// We're inside the outer ( from &&(, so depth starts at 1.
	// The expression ends at a comma at depth 1 or a closing paren at depth 0.
	exprStart := m[1] // position right after the full match "PARAM&&(PARAM="
	depth := 1
	pos := exprStart
	for pos < len(rest) {
		ch := rest[pos]
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				// Reached the end without finding a comma — expression is the whole thing
				goto done
			}
		case ',':
			if depth == 1 {
				goto done
			}
		case '\'', '"':
			// Skip string literals to avoid matching parentheses/commas inside strings
			pos = skipStringLiteral(rest, pos)
			continue
		}
		pos++
	}
done:
	if pos <= exprStart {
		return ""
	}

	transform := rest[exprStart:pos]

	// Replace the parameter name with "sig" using word-boundary matching
	paramRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(param) + `\b`)
	return paramRe.ReplaceAllString(transform, "sig")
}

// preprocessPlayerFull includes the entire player.js with solver bindings
// inserted inside the IIFE. This is the robust approach that handles YouTube's
// modern obfuscation (string tables, combined multipurpose functions, etc.).
func preprocessPlayerFull(playerJS string) (string, error) {
	// Build n-param generator expressions.
	// Array candidates (older players: ;X=[func];)
	var nGenerators []string
	for _, name := range findNArrayCandidates(playerJS) {
		nGenerators = append(nGenerators, fmt.Sprintf("function(n){return %s(n)}", name))
	}

	// Build sig generator expressions.
	var sigGenerators []string
	// Old sig candidate pattern (older players)
	for _, c := range findSigCandidates(playerJS) {
		sigGenerators = append(sigGenerators, fmt.Sprintf(
			"function(sig){return %s(%s,sig)}", c.funcName, c.literal,
		))
	}
	// New: transform chain from set("alr","yes") marker (this IS the sig function)
	if sigChain := findAlrTransformChain(playerJS); sigChain != "" {
		sigGenerators = append(sigGenerators, fmt.Sprintf("function(sig){return %s}", sigChain))
	}

	if len(nGenerators) == 0 && len(sigGenerators) == 0 {
		return "", fmt.Errorf("no n-param or sig candidates found in player JS (size=%d)", len(playerJS))
	}

	// Find the IIFE closing point to insert bindings inside the function scope.
	// Main variant: var _yt_player={};(function(g){...})(_yt_player);
	// TV variant: 'use strict';(function(){var window=this;...}).call(this);
	closeIdx := strings.LastIndex(playerJS, "})(")
	if closeIdx < 0 {
		// TV/ES6 variant uses }).call(this) instead of })(arg)
		closeIdx = strings.LastIndex(playerJS, "}).call(")
	}
	if closeIdx < 0 {
		return "", fmt.Errorf("could not find IIFE closing bracket")
	}

	// Build solver binding code
	bindingCode := buildSolverBindings(nGenerators, sigGenerators)

	// Insert bindings inside the IIFE, just before the closing })
	modified := playerJS[:closeIdx] + "\n" + bindingCode + "\n" + playerJS[closeIdx:]

	// Strip "var window=this;" to avoid overriding our setup stubs.
	// The TS version does the same (removes this line from the IIFE body).
	modified = strings.Replace(modified, "var window=this;", "", 1)

	// Prepend setup stubs
	return fullPlayerSetupCode + "\n" + modified, nil
}

// buildSolverBindings generates JS code that assigns solver functions to _result.
// nGenerators and sigGenerators are pre-built JS function expressions.
func buildSolverBindings(nGenerators, sigGenerators []string) string {
	var parts []string

	// N-param: single IIFE that tries URL builder first (newer players where
	// n-param transform is embedded in g.fb serialization), then falls back to
	// validated array candidates (older players with standalone n-param functions).
	nCandidatesJS := "[]"
	if len(nGenerators) > 0 {
		nCandidatesJS = "[" + strings.Join(nGenerators, ",") + "]"
	}
	parts = append(parts, fmt.Sprintf(nBindingTemplate, nCandidatesJS))

	if len(sigGenerators) > 0 {
		parts = append(parts, fmt.Sprintf(
			"_result.sig = _multiTry([%s]);",
			strings.Join(sigGenerators, ","),
		))
	}

	return strings.Join(parts, "\n")
}

// nBindingTemplate is a JS IIFE that finds the n-param transform function.
// It tries two strategies:
//  1. URL builder serialization (newer players): creates a g.fb instance with a
//     known n-param, serializes it, and checks if the value was transformed.
//  2. Validated array candidates (older players): tests each candidate with a
//     dummy value and only accepts candidates that produce a different string.
//
// Falls back to null (passthrough) if neither strategy works.
// The %s placeholder receives the array of candidate function expressions.
const nBindingTemplate = `
_result.n = (function() {
  try {
    var fb = g && g.fb;
    if (fb) {
      var testN = "AAAAAA_TESTN_VAL";
      var u = new fb("https://rr1---sn-a.googlevideo.com/videoplayback?n=" + testN, true);
      var s = (typeof u.A_ === "function") ? u.A_() : "" + u;
      if (typeof s === "string") {
        var m = s.match(/[?&]n=([^&]+)/);
        if (m && m[1] && m[1] !== testN) {
          return function(input) {
            var url = new fb("https://rr1---sn-a.googlevideo.com/videoplayback?n=" + input, true);
            var result = (typeof url.A_ === "function") ? url.A_() : "" + url;
            var match = result.match(/[?&]n=([^&]+)/);
            return (match && match[1]) ? match[1] : input;
          };
        }
      }
    }
  } catch(e) {}
  var _cands = %s;
  for (var _i = 0; _i < _cands.length; _i++) {
    try {
      var _test = _cands[_i]("AAAA_VALIDATE");
      if (typeof _test === "string" && _test.length > 0 && _test !== "AAAA_VALIDATE") {
        return _cands[_i];
      }
    } catch(e) {}
  }
  return null;
})();
`

// fullPlayerSetupCode provides comprehensive browser stubs for running the full
// YouTube player.js in Goja. More comprehensive than the legacy setupCode since
// the full player code references many more browser APIs.
const fullPlayerSetupCode = `
var _multiTry = function(_generators) {
    return function(_input) {
        var _errors = [];
        for (var _i = 0; _i < _generators.length; _i++) {
            try {
                var _r = _generators[_i](_input);
                if (typeof _r === "string" && _r.length > 0) return _r;
                _errors.push("candidate " + _i + ": returned " + (typeof _r) + " " + JSON.stringify(_r));
            } catch(_e) {
                _errors.push("candidate " + _i + ": " + (_e.message || _e));
            }
        }
        throw new Error("no cipher solutions found (" + _generators.length + " candidates tried: " + _errors.join("; ") + ")");
    };
};
if (typeof globalThis.XMLHttpRequest === "undefined") {
    globalThis.XMLHttpRequest = function() {};
    globalThis.XMLHttpRequest.prototype = {open:function(){},send:function(){},setRequestHeader:function(){}};
}
if (typeof window === "undefined") {
    var window = Object.create(null);
}
window.location = {
    hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
    href: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
    origin: "https://www.youtube.com", pathname: "/watch",
    port: "", protocol: "https:", search: "?v=dQw4w9WgXcQ",
    password: "", username: "", toString: function() { return this.href; }
};
if (typeof document === "undefined") {
    var document = Object.create(null);
    document.addEventListener = function(){};
    document.createElement = function(t){ return {tagName:t,style:{}}; };
    document.getElementById = function(){ return null; };
    document.querySelector = function(){ return null; };
    document.querySelectorAll = function(){ return []; };
}
if (typeof navigator === "undefined") {
    var navigator = Object.create(null);
    navigator.userAgent = "Mozilla/5.0";
}
if (typeof self === "undefined") {
    var self = globalThis;
}
if (typeof location === "undefined") {
    var location = window.location;
}
if (typeof fetch === "undefined") {
    var fetch = function() { return Promise.reject("no fetch"); };
}
if (typeof AbortController === "undefined") {
    var AbortController = function() { this.signal = {}; this.abort = function(){}; };
}
if (typeof ReadableStream === "undefined") {
    var ReadableStream = function() {};
}
if (typeof CustomEvent === "undefined") {
    var CustomEvent = function(t,o) { this.type = t; };
}
if (typeof MutationObserver === "undefined") {
    var MutationObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof ResizeObserver === "undefined") {
    var ResizeObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof IntersectionObserver === "undefined") {
    var IntersectionObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof matchMedia === "undefined") {
    var matchMedia = function() { return {matches:false,addListener:function(){},addEventListener:function(){}}; };
}
if (typeof requestAnimationFrame === "undefined") {
    var requestAnimationFrame = function(cb) { return 0; };
    var cancelAnimationFrame = function() {};
}
if (typeof getComputedStyle === "undefined") {
    var getComputedStyle = function() { return {}; };
}
if (typeof CSS === "undefined") {
    var CSS = {supports: function() { return false; }};
}
if (typeof performance === "undefined") {
    var performance = {now: function() { return Date.now(); }, mark: function(){}, measure: function(){}};
}
if (typeof Intl === "undefined") {
    var _intlProto = {resolvedOptions:function(){return {timeZone:"UTC",locale:"en"};},format:function(){return "";}};
    var _intlCtor = function(){return Object.create(_intlProto);};
    _intlCtor.supportedLocalesOf = function(){return [];};
    var Intl = {DateTimeFormat:_intlCtor,NumberFormat:_intlCtor,PluralRules:_intlCtor,RelativeTimeFormat:_intlCtor,Collator:_intlCtor,ListFormat:_intlCtor,Segmenter:_intlCtor};
}
if (typeof queueMicrotask === "undefined") {
    var queueMicrotask = function(fn) { Promise.resolve().then(fn); };
}
if (typeof TextEncoder === "undefined") {
    var TextEncoder = function() { this.encode = function(s) { return new Uint8Array(0); }; };
}
if (typeof TextDecoder === "undefined") {
    var TextDecoder = function() { this.decode = function() { return ""; }; };
}
`

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

// windowThisPattern matches "var window=this;" with flexible whitespace.
var windowThisPattern = regexp.MustCompile(`var\s+window\s*=\s*this\s*;`)

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

// urlClassPatterns match the n-param URL class by finding the pattern:
//   new XXXX(url, !0)).get("n")  or  new XXXX(url, true)).get("n")
// The class name changes across player versions (e.g., g.fb → g.sB).
// Patterns are tried in priority order: dotted (!0), dotted (true), bare.
var urlClassPatterns = []*regexp.Regexp{
	// Dotted identifier with !0: new g.sB(Z, !0)).get("n")
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*!0\)\)\.get\("n"\)`),
	// Dotted identifier with true: new g.sB(Z, true)).get("n")
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*true\)\)\.get\("n"\)`),
	// Bare identifier: new UrlBuilder(Z, !0)).get("n") or true
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*)\([^,]+,\s*(?:!0|true)\)\)\.get\("n"\)`),
}

// findURLClassName extracts the URL builder class name (e.g., "g.sB", "h.Foo")
// from the player JS by finding the n-param resolver pattern.
// Tries dotted identifiers first, then bare identifiers.
func findURLClassName(playerJS string) string {
	for _, pattern := range urlClassPatterns {
		m := pattern.FindStringSubmatch(playerJS)
		if m != nil {
			return m[1]
		}
	}
	return ""
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

// alrMarkers are the string markers used to identify URL builder functions
// that contain the signature decipher chain. Both quote styles are checked.
var alrMarkers = []string{
	`set("alr","yes");`,
	`set('alr','yes');`,
}

// alrTransformHeadPattern matches the start of a transform chain after set("alr","yes").
// Captures the parameter name and the start of the assignment.
var alrTransformHeadPattern = regexp.MustCompile(`(\w+)&&\((\w+)=`)

// alrProximity is the maximum number of characters after an ALR marker to search
// for the transform head pattern. The correct match is typically within 15 chars;
// 200 gives ample room for formatting variations without matching unrelated code.
const alrProximity = 200

// findAlrTransformChains finds signature decipher chains in YouTube's newer
// player.js format. The URL builder function is identified by its set("alr","yes")
// marker. The transform chain after it applies signature decryption.
//
// Returns all valid transform chains found (one per ALR marker with a nearby
// transform pattern). Each chain has the parameter replaced with "sig".
func findAlrTransformChains(playerJS string) []string {
	var chains []string

	for _, marker := range alrMarkers {
		offset := 0
		for {
			idx := strings.Index(playerJS[offset:], marker)
			if idx < 0 {
				break
			}
			absIdx := offset + idx
			afterMarker := absIdx + len(marker)
			offset = afterMarker // advance for next iteration

			// Only search within alrProximity chars of the marker
			searchEnd := afterMarker + alrProximity
			if searchEnd > len(playerJS) {
				searchEnd = len(playerJS)
			}
			nearby := playerJS[afterMarker:searchEnd]

			m := alrTransformHeadPattern.FindStringSubmatchIndex(nearby)
			if m == nil {
				continue
			}

			param := nearby[m[2]:m[3]]
			assignParam := nearby[m[4]:m[5]]
			if param != assignParam {
				continue
			}

			// Extract the transform expression by tracking parenthesis depth.
			// Use the full remaining text (not just nearby) since the expression
			// itself can be longer than alrProximity.
			fullRest := playerJS[afterMarker:]
			exprStart := m[1] // right after "PARAM&&(PARAM="
			depth := 1
			pos := exprStart
			for pos < len(fullRest) {
				ch := fullRest[pos]
				switch ch {
				case '(':
					depth++
				case ')':
					depth--
					if depth == 0 {
						goto done
					}
				case ',':
					if depth == 1 {
						goto done
					}
				case '\'', '"':
					pos = skipStringLiteral(fullRest, pos)
					continue
				}
				pos++
			}
			// Loop exhausted without finding terminator — unbalanced expression
			continue
		done:
			if pos <= exprStart {
				continue
			}

			transform := fullRest[exprStart:pos]

			// Replace the parameter name with "sig" using word-boundary matching
			paramRe, err := regexp.Compile(`\b` + regexp.QuoteMeta(param) + `\b`)
			if err != nil {
				continue
			}
			chain := paramRe.ReplaceAllString(transform, "sig")
			chains = append(chains, chain)
		}
	}

	return chains
}

// preprocessPlayerFull includes the entire player.js with solver bindings
// inserted inside the IIFE. This is the robust approach that handles YouTube's
// modern obfuscation (string tables, combined multipurpose functions, etc.).
func preprocessPlayerFull(playerJS string) (string, error) {
	// Find the URL builder class name (e.g., "g.sB", "g.fb") for n-param resolution.
	// The class name changes across player versions; we detect it dynamically.
	urlClassName := findURLClassName(playerJS)

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
	// New: transform chains from set("alr","yes") markers
	for _, sigChain := range findAlrTransformChains(playerJS) {
		sigGenerators = append(sigGenerators, fmt.Sprintf("function(sig){return %s}", sigChain))
	}

	if len(nGenerators) == 0 && len(sigGenerators) == 0 && urlClassName == "" {
		return "", fmt.Errorf("no n-param or sig candidates found in player JS (size=%d)", len(playerJS))
	}

	// Find the IIFE closing point to insert bindings inside the function scope.
	// Main variant: var _yt_player={};(function(g){...})(_yt_player);
	// TV variant: 'use strict';(function(){var window=this;...}).call(this);
	closeIdx := strings.LastIndex(playerJS, "})(")
	if closeIdx >= 0 && len(playerJS)-closeIdx > 200 {
		// Match is too far from EOF — likely inside a string or nested function
		closeIdx = -1
	}
	if closeIdx < 0 {
		// TV/ES6 variant uses }).call(this) instead of })(arg)
		closeIdx = strings.LastIndex(playerJS, "}).call(")
		if closeIdx >= 0 && len(playerJS)-closeIdx > 200 {
			closeIdx = -1
		}
	}
	if closeIdx < 0 {
		return "", fmt.Errorf("could not find IIFE closing bracket")
	}

	// Build solver binding code
	bindingCode := buildSolverBindings(nGenerators, sigGenerators, urlClassName)

	// Insert bindings inside the IIFE, just before the closing })
	modified := playerJS[:closeIdx] + "\n" + bindingCode + "\n" + playerJS[closeIdx:]

	// Strip "var window=this;" (with flexible whitespace) to avoid overriding our setup stubs.
	modified = windowThisPattern.ReplaceAllLiteralString(modified, "")

	// Prepend setup stubs
	return fullPlayerSetupCode + "\n" + modified, nil
}

// buildSolverBindings generates JS code that assigns solver functions to _result.
// nGenerators and sigGenerators are pre-built JS function expressions.
// urlClassName is the dynamically-detected URL builder class (e.g., "g.sB").
func buildSolverBindings(nGenerators, sigGenerators []string, urlClassName string) string {
	var parts []string

	// N-param: single IIFE that tries URL builder first (newer players where
	// n-param transform is embedded in URL class serialization), then falls back
	// to validated array candidates (older players with standalone n-param functions).
	nCandidatesJS := "[]"
	if len(nGenerators) > 0 {
		nCandidatesJS = "[" + strings.Join(nGenerators, ",") + "]"
	}
	// The URL class name changes across player versions (g.fb, g.sB, etc.)
	// We detect it from the player JS and inject it; empty means not found.
	urlClassJS := "null"
	if urlClassName != "" {
		urlClassJS = urlClassName
	}
	parts = append(parts, fmt.Sprintf(nBindingTemplate, urlClassJS, nCandidatesJS))

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
//  1. URL builder class (newer players): uses the dynamically-detected URL class
//     (e.g., g.sB) which internally transforms n-params when constructed with the
//     second arg = true. The class name is injected as the first %s placeholder.
//  2. Validated array candidates (older players): tests each candidate with a
//     dummy value and only accepts candidates that produce a different string.
//
// Falls back to null (passthrough) if neither strategy works.
// First %s = URL class reference (e.g., g.sB), second %s = array of candidates.
const nBindingTemplate = `
_result.n = (function() {
  try {
    var _urlClass = %s;
    if (_urlClass) {
      var testN = "AAAAAA_TESTN_VAL";
      var u = new _urlClass("https://rr1---sn-a.googlevideo.com/videoplayback?n=" + testN, true);
      var s = u.get ? u.get("n") : ((typeof u.A_ === "function") ? u.A_() : "" + u);
      if (u.get) {
        if (typeof s === "string" && s.length > 0 && s !== testN) {
          return function(input) {
            var url = new _urlClass("https://rr1---sn-a.googlevideo.com/videoplayback?n=" + input, true);
            return url.get("n") || input;
          };
        }
      } else if (typeof s === "string") {
        var m = s.match(/[?&]n=([^&]+)/);
        if (m && m[1] && m[1] !== testN) {
          return function(input) {
            var url = new _urlClass("https://rr1---sn-a.googlevideo.com/videoplayback?n=" + input, true);
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
                if (typeof _r === "string" && _r.length > 0 && _r !== _input) return _r;
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
globalThis.location = {
    hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
    href: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
    origin: "https://www.youtube.com", pathname: "/watch",
    port: "", protocol: "https:", search: "?v=dQw4w9WgXcQ",
    password: "", username: "", toString: function() { return this.href; }
};
if (typeof globalThis.document === "undefined") {
    globalThis.document = Object.create(null);
    globalThis.document.addEventListener = function(){};
    globalThis.document.createElement = function(t){ return {tagName:t,style:{}}; };
    globalThis.document.getElementById = function(){ return null; };
    globalThis.document.querySelector = function(){ return null; };
    globalThis.document.querySelectorAll = function(){ return []; };
}
if (typeof globalThis.navigator === "undefined") {
    globalThis.navigator = Object.create(null);
    globalThis.navigator.userAgent = "Mozilla/5.0";
}
if (typeof globalThis.self === "undefined") {
    globalThis.self = globalThis;
}
if (typeof globalThis.window === "undefined") {
    globalThis.window = globalThis;
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

package cipher

import (
	"fmt"
	"regexp"
	"strings"
)

// sigCandidate represents a sig decryption function call with its control code.
type sigCandidate struct {
	funcName string
	literal  string // numeric control code passed as first arg
}

// --- New "full player" approach (matches TypeScript AST-based method) ---
// Instead of extracting individual functions with regex (fragile),
// we include the ENTIRE player.js with solver bindings inserted.
// This mirrors the TS approach: parse full player → find candidates → append bindings.

// findNArrayCandidates finds n-parameter function candidates by searching for
// single-element array assignments: varName=[funcName];
// This matches the TypeScript AST pattern: ArrayExpression with 1 Identifier element.
var nArrayPattern = regexp.MustCompile(`;([a-zA-Z_$][\w$]*)\s*=\s*\[([a-zA-Z_$][\w$]*)\]\s*;`)

func findNArrayCandidates(playerJS string) []string {
	matches := nArrayPattern.FindAllStringSubmatch(playerJS, -1)
	var names []string
	seen := make(map[string]bool)
	for _, m := range matches {
		funcName := m[2]
		if !seen[funcName] {
			seen[funcName] = true
			names = append(names, funcName)
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

// preprocessPlayerFull includes the entire player.js with solver bindings
// inserted inside the IIFE. This is the robust approach that handles YouTube's
// modern obfuscation (string tables, combined multipurpose functions, etc.).
func preprocessPlayerFull(playerJS string) (string, error) {
	// Find n-param candidates
	nFuncs := findNArrayCandidates(playerJS)

	// Find sig candidates
	sigCands := findSigCandidates(playerJS)

	if len(nFuncs) == 0 && len(sigCands) == 0 {
		return "", fmt.Errorf("no n-param or sig candidates found in player JS")
	}

	// Find the IIFE closing point to insert bindings inside the function scope.
	// Player JS structure: var _yt_player={};(function(g){...})(_yt_player);
	closeIdx := strings.LastIndex(playerJS, "})(")
	if closeIdx < 0 {
		return "", fmt.Errorf("could not find IIFE closing bracket")
	}

	// Build solver binding code
	bindingCode := buildSolverBindings(nFuncs, sigCands)

	// Insert bindings inside the IIFE, just before the closing })
	modified := playerJS[:closeIdx] + "\n" + bindingCode + "\n" + playerJS[closeIdx:]

	// Strip "var window=this;" to avoid overriding our setup stubs.
	// The TS version does the same (removes this line from the IIFE body).
	modified = strings.Replace(modified, "var window=this;", "", 1)

	// Prepend setup stubs
	return fullPlayerSetupCode + "\n" + modified, nil
}

// buildSolverBindings generates JS code that assigns solver functions to _result.
// Uses a multiTry pattern (like the TS version) to handle multiple candidates.
func buildSolverBindings(nFuncs []string, sigCands []sigCandidate) string {
	var parts []string

	if len(nFuncs) > 0 {
		var generators []string
		for _, name := range nFuncs {
			generators = append(generators, fmt.Sprintf("function(n){return %s(n)}", name))
		}
		parts = append(parts, fmt.Sprintf(
			"_result.n = _multiTry([%s]);",
			strings.Join(generators, ","),
		))
	}

	if len(sigCands) > 0 {
		var generators []string
		for _, c := range sigCands {
			generators = append(generators, fmt.Sprintf(
				"function(sig){return %s(%s,sig)}", c.funcName, c.literal,
			))
		}
		parts = append(parts, fmt.Sprintf(
			"_result.sig = _multiTry([%s]);",
			strings.Join(generators, ","),
		))
	}

	return strings.Join(parts, "\n")
}

// fullPlayerSetupCode provides comprehensive browser stubs for running the full
// YouTube player.js in Goja. More comprehensive than the legacy setupCode since
// the full player code references many more browser APIs.
const fullPlayerSetupCode = `
var _multiTry = function(_generators) {
    return function(_input) {
        for (var _i = 0; _i < _generators.length; _i++) {
            try {
                var _r = _generators[_i](_input);
                if (typeof _r === "string" && _r.length > 0) return _r;
            } catch(_e) {}
        }
        throw new Error("no cipher solutions found");
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

// --- Legacy regex-based approach (fallback for older player.js versions) ---

// Regex patterns for finding the signature decipher function name in player JS.
// YouTube's player JS changes frequently, so we try multiple patterns.
var sigFunctionNamePatterns = []*regexp.Regexp{
	// c&&(c=XX.sig||YY(c))
	regexp.MustCompile(`\b[cs]\s*&&\s*[adf]\.set\([^,]+\s*,\s*encodeURIComponent\(([a-zA-Z0-9$]+)\(`),
	// b.set("signature", XX(c))
	regexp.MustCompile(`\b[a-zA-Z0-9]+\s*&&\s*[a-zA-Z0-9]+\.set\([^,]+\s*,\s*encodeURIComponent\(([a-zA-Z0-9$]+)\(`),
	// m=XX(decodeURIComponent(h.s))
	regexp.MustCompile(`\bm=([a-zA-Z0-9$]{2,})\(decodeURIComponent\(h\.s\)\)`),
	// c&&d.set(e, encodeURIComponent(XX(
	regexp.MustCompile(`\bc\s*&&\s*d\.set\([^,]+\s*,\s*(?:encodeURIComponent\s*\()([a-zA-Z0-9$]+)\(`),
	// c&&f.set(e, XX(
	regexp.MustCompile(`\bc\s*&&\s*[a-z]\.set\([^,]+\s*,\s*([a-zA-Z0-9$]+)\(`),
	// c&&f.set(e, encodeURIComponent(XX(
	regexp.MustCompile(`\bc\s*&&\s*[a-z]\.set\([^,]+\s*,\s*encodeURIComponent\(([a-zA-Z0-9$]+)\(`),
}

// sigFunctionBodyPatternTemplates generates regex patterns for the signature function body.
// Go's regexp doesn't support backreferences (\1), so we generate patterns for each
// common parameter name (a-z) that YouTube's obfuscator uses.
func sigFunctionBodyPatternTemplates(funcName string) []*regexp.Regexp {
	escapedName := regexp.QuoteMeta(funcName)
	var patterns []*regexp.Regexp
	// YouTube typically uses single-letter parameter names: a, b, c, etc.
	for _, param := range "abcdefgh" {
		p := string(param)
		// var XX=function(a){a=a.split("");...;return a.join("")}
		pat1 := fmt.Sprintf(`(?:var\s+)?%s\s*=\s*function\(\s*%s\s*\)\s*\{%s=%s\.split\(""\);[^}]*;return\s+%s\.join\(""\)\}`,
			escapedName, p, p, p, p)
		if re, err := regexp.Compile(pat1); err == nil {
			patterns = append(patterns, re)
		}
		// function XX(a){a=a.split("");...;return a.join("")}
		pat2 := fmt.Sprintf(`function\s+%s\s*\(\s*%s\s*\)\s*\{%s=%s\.split\(""\);[^}]*;return\s+%s\.join\(""\)\}`,
			escapedName, p, p, p, p)
		if re, err := regexp.Compile(pat2); err == nil {
			patterns = append(patterns, re)
		}
	}
	return patterns
}

// Regex patterns for finding the n-parameter transform function name.
var nFunctionNamePatterns = []*regexp.Regexp{
	// var b=a.get("n");if(b){b=XX[0](b)
	regexp.MustCompile(`\.get\("n"\)\)&&\(b=([a-zA-Z0-9$]+)(?:\[(\d+)\])?\(b\)`),
	// var b=a.get("n");b&&(b=XX[0](b),a.set("n",b))
	regexp.MustCompile(`\("n"\)\s*&&\s*\(b\s*=\s*([a-zA-Z0-9$]+)(?:\[(\d+)\])?\(b\)`),
	// b=String.fromCharCode(110);c=a.get(b);if(c){d=XX[0](c)
	regexp.MustCompile(`\(\s*"[a-zA-Z]"\s*\);\s*\w+\s*=\s*\w+\.get\(\s*\w+\s*\);.+?(?:&&|if)\s*\(\s*\w+\s*\)\s*\{?\s*\w+\s*=\s*([a-zA-Z0-9$]+)\s*(?:\[\s*(\d+)\s*\])?\s*\(\s*\w+\s*\)`),
}

// extractSigFunction extracts the signature decipher function and its helper object from player JS.
// Returns executable JS code: the helper object + decipher function, wrapped for calling.
func extractSigFunction(playerJS string) (string, error) {
	// Step 1: Find the function name
	funcName := ""
	for _, pattern := range sigFunctionNamePatterns {
		m := pattern.FindStringSubmatch(playerJS)
		if m != nil {
			funcName = m[1]
			break
		}
	}
	if funcName == "" {
		return "", fmt.Errorf("could not find signature function name")
	}

	// Step 2: Extract the function body using generated patterns (no backreferences)
	var funcBody string

	for _, re := range sigFunctionBodyPatternTemplates(funcName) {
		m := re.FindString(playerJS)
		if m != "" {
			funcBody = m
			break
		}
	}

	// Fallback: try to extract using balanced brace matching
	if funcBody == "" {
		body, err := extractFunctionByName(playerJS, funcName)
		if err != nil {
			return "", fmt.Errorf("could not extract sig function body for %q: %w", funcName, err)
		}
		funcBody = body
	}

	// Step 3: Find the helper object name from the function body
	// The function body calls methods on a helper object like: XX.YY(a,ZZ)
	helperPattern := regexp.MustCompile(`([a-zA-Z_$][\w$]*)\.\w+\(\s*\w`)
	helperMatch := helperPattern.FindStringSubmatch(funcBody)
	if helperMatch == nil {
		return "", fmt.Errorf("could not find helper object in sig function")
	}
	helperName := helperMatch[1]

	// Step 4: Extract the helper object definition
	helperBody, err := extractObjectByName(playerJS, helperName)
	if err != nil {
		return "", fmt.Errorf("could not extract helper object %q: %w", helperName, err)
	}

	// Step 5: Build executable code
	code := fmt.Sprintf(`%s
%s
function _decipher(sig) {
    return %s(sig);
}`, helperBody, funcBody, funcName)

	return code, nil
}

// extractNFunction extracts the n-parameter transform function from player JS.
// Returns executable JS code wrapping the n-transform function.
func extractNFunction(playerJS string) (string, error) {
	// Step 1: Find the function name (and optional array index)
	funcName := ""
	arrayIdx := ""
	for _, pattern := range nFunctionNamePatterns {
		m := pattern.FindStringSubmatch(playerJS)
		if m != nil {
			funcName = m[1]
			if len(m) > 2 {
				arrayIdx = m[2]
			}
			break
		}
	}
	if funcName == "" {
		return "", fmt.Errorf("could not find n-parameter function name")
	}

	// If there's an array index, we need to find the actual function name from the array
	if arrayIdx != "" {
		actualName, err := resolveArrayFunction(playerJS, funcName, arrayIdx)
		if err == nil && actualName != "" {
			funcName = actualName
		}
	}

	// Step 2: Extract the function body using balanced brace matching
	funcBody, err := extractFunctionByName(playerJS, funcName)
	if err != nil {
		return "", fmt.Errorf("could not extract n function body for %q: %w", funcName, err)
	}

	// Step 3: Build executable code with setup stubs
	code := fmt.Sprintf(`%s
function _nTransform(n) {
    return %s(n);
}`, funcBody, funcName)

	return code, nil
}

// extractFunctionByName extracts a function definition by name using balanced brace matching.
// Handles both: var XX=function(...){...} and function XX(...){...}
func extractFunctionByName(js, name string) (string, error) {
	escapedName := regexp.QuoteMeta(name)

	// Try: var XX=function(
	patterns := []string{
		fmt.Sprintf(`(?:var\s+)?%s\s*=\s*function\s*\(`, escapedName),
		fmt.Sprintf(`function\s+%s\s*\(`, escapedName),
	}

	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		loc := re.FindStringIndex(js)
		if loc == nil {
			continue
		}

		start := loc[0]
		// Find the opening brace
		braceStart := strings.Index(js[loc[1]:], "{")
		if braceStart < 0 {
			continue
		}
		braceStart += loc[1]

		// Find matching closing brace (handles strings/regex)
		end := findMatchingBrace(js, braceStart)
		if end < 0 {
			continue
		}
		end++ // include the closing brace

		// Check for trailing semicolon
		if end < len(js) && js[end] == ';' {
			end++
		}

		return js[start:end], nil
	}

	return "", fmt.Errorf("function %q not found", name)
}

// findMatchingBrace finds the position of the closing brace matching the opening
// brace at position start, properly skipping string literals and regex literals.
func findMatchingBrace(js string, start int) int {
	depth := 0
	i := start
	for i < len(js) {
		ch := js[i]
		switch ch {
		case '{':
			depth++
			i++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
			i++
		case '\'', '"', '`':
			// Skip string literal
			i = skipStringLiteral(js, i)
		case '/':
			// Could be regex literal, line comment, or block comment
			if i+1 < len(js) {
				if js[i+1] == '/' {
					// Line comment — skip to end of line
					for i < len(js) && js[i] != '\n' {
						i++
					}
				} else if js[i+1] == '*' {
					// Block comment — skip to */
					i += 2
					for i+1 < len(js) {
						if js[i] == '*' && js[i+1] == '/' {
							i += 2
							break
						}
						i++
					}
				} else {
					// Could be regex literal — skip it
					i = skipRegexLiteral(js, i)
				}
			} else {
				i++
			}
		default:
			i++
		}
	}
	return -1
}

// skipStringLiteral advances past a string literal starting at position i.
func skipStringLiteral(js string, i int) int {
	quote := js[i]
	i++ // skip opening quote
	for i < len(js) {
		if js[i] == '\\' {
			i += 2 // skip escaped char
			continue
		}
		if js[i] == quote {
			if quote == '`' {
				// Template literals are fine, just skip
			}
			return i + 1
		}
		i++
	}
	return i
}

// skipRegexLiteral advances past a regex literal starting at position i.
func skipRegexLiteral(js string, i int) int {
	i++ // skip opening /
	for i < len(js) {
		if js[i] == '\\' {
			i += 2 // skip escaped char
			continue
		}
		if js[i] == '/' {
			i++ // skip closing /
			// Skip flags
			for i < len(js) && (js[i] >= 'a' && js[i] <= 'z') {
				i++
			}
			return i
		}
		if js[i] == '\n' {
			return i // not a regex, bail
		}
		i++
	}
	return i
}

// extractObjectByName extracts an object literal definition by name using balanced brace matching.
// Matches: var XX={...};
func extractObjectByName(js, name string) (string, error) {
	escapedName := regexp.QuoteMeta(name)
	pattern := fmt.Sprintf(`(?:var\s+)?%s\s*=\s*\{`, escapedName)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}

	loc := re.FindStringIndex(js)
	if loc == nil {
		return "", fmt.Errorf("object %q not found", name)
	}

	start := loc[0]
	// Find the opening brace
	braceStart := strings.Index(js[loc[0]:], "{")
	if braceStart < 0 {
		return "", fmt.Errorf("object %q: no opening brace", name)
	}
	braceStart += loc[0]

	end := findMatchingBrace(js, braceStart)
	if end < 0 {
		return "", fmt.Errorf("object %q: unbalanced braces", name)
	}
	end++ // include closing brace

	// Check for trailing semicolon
	if end < len(js) && js[end] == ';' {
		end++
	}

	return js[start:end], nil
}

// resolveArrayFunction resolves a function name from an array variable.
// e.g., var XX=[func1,func2]; with index 0 -> returns "func1"
func resolveArrayFunction(js, arrayName, index string) (string, error) {
	escapedName := regexp.QuoteMeta(arrayName)
	// Match: var XX=[...];
	pattern := fmt.Sprintf(`(?:var\s+)?%s\s*=\s*\[([^\]]+)\]`, escapedName)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}

	m := re.FindStringSubmatch(js)
	if m == nil {
		return "", fmt.Errorf("array %q not found", arrayName)
	}

	elements := strings.Split(m[1], ",")
	idx := 0
	if index != "" {
		fmt.Sscanf(index, "%d", &idx)
	}

	if idx < 0 || idx >= len(elements) {
		return "", fmt.Errorf("array index %d out of range (len=%d)", idx, len(elements))
	}

	return strings.TrimSpace(elements[idx]), nil
}

// preprocessPlayer extracts sig and n functions from player JS and generates
// standalone executable JS code. Returns code that when executed sets _result.n and _result.sig.
//
// Tries the "full player" approach first (include entire player.js with solver bindings),
// which handles YouTube's modern obfuscation. Falls back to legacy regex extraction
// for older player.js versions.
func preprocessPlayer(playerJS string) (string, error) {
	// Try new "full player" approach first (robust against modern obfuscation)
	code, err := preprocessPlayerFull(playerJS)
	if err == nil {
		return code, nil
	}

	// Fall back to legacy regex-based extraction
	return preprocessPlayerLegacy(playerJS)
}

// preprocessPlayerLegacy is the original regex-based approach for older player.js versions.
func preprocessPlayerLegacy(playerJS string) (string, error) {
	var parts []string

	// Setup: browser stubs for the extracted code
	parts = append(parts, setupCode)

	// Extract sig function
	sigCode, sigErr := extractSigFunction(playerJS)
	if sigErr == nil && sigCode != "" {
		parts = append(parts, sigCode)
		parts = append(parts, `_result.sig = function(input) { return _decipher(input); };`)
	}

	// Extract n function
	nCode, nErr := extractNFunction(playerJS)
	if nErr == nil && nCode != "" {
		parts = append(parts, nCode)
		parts = append(parts, `_result.n = function(input) { return _nTransform(input); };`)
	}

	if sigErr != nil && nErr != nil {
		return "", fmt.Errorf("failed to extract both sig (%v) and n (%v)", sigErr, nErr)
	}

	return strings.Join(parts, "\n"), nil
}

// setupCode provides browser-like stubs to prevent ReferenceError when
// executing extracted YouTube player functions.
const setupCode = `
if (typeof globalThis.XMLHttpRequest === "undefined") {
    globalThis.XMLHttpRequest = function() {};
    globalThis.XMLHttpRequest.prototype = {};
}
if (typeof window === "undefined") {
    var window = Object.create(null);
    window.location = {
        hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
        href: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
        origin: "https://www.youtube.com", pathname: "/watch",
        port: "", protocol: "https:", search: "?v=dQw4w9WgXcQ"
    };
}
if (typeof document === "undefined") {
    var document = Object.create(null);
}
if (typeof navigator === "undefined") {
    var navigator = Object.create(null);
}
if (typeof self === "undefined") {
    var self = globalThis;
}
if (typeof location === "undefined") {
    var location = window.location || {};
}
`

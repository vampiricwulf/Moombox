package cipher

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// --- Legacy regex-based approach (fallback for older player.js versions) ---

// legacySigHelperPattern matches helper object method calls in legacy sig
// function bodies. Only the legacy extractor uses this; kept alongside its
// consumer so the full-player path has a cleaner surface.
var legacySigHelperPattern = regexp.MustCompile(`([a-zA-Z_$][\w$]*)\.\w+\(\s*\w`)

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
		return "", fmt.Errorf("%w: could not find signature function name", ErrExtractorMismatch)
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
	helperPattern := legacySigHelperPattern
	helperMatch := helperPattern.FindStringSubmatch(funcBody)
	if helperMatch == nil {
		return "", fmt.Errorf("%w: could not find helper object in sig function", ErrExtractorMismatch)
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
		return "", fmt.Errorf("%w: could not find n-parameter function name", ErrExtractorMismatch)
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
		return "", fmt.Errorf("%w: array %q not found", ErrExtractorMismatch, arrayName)
	}

	elements := strings.Split(m[1], ",")
	idx := 0
	if index != "" {
		if parsed, err := strconv.Atoi(index); err == nil {
			idx = parsed
		}
	}

	if idx < 0 || idx >= len(elements) {
		return "", fmt.Errorf("%w: array index %d out of range (len=%d)", ErrExtractorMismatch, idx, len(elements))
	}

	return strings.TrimSpace(elements[idx]), nil
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
if (typeof globalThis.location === "undefined") {
    globalThis.location = {
        hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
        href: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
        origin: "https://www.youtube.com", pathname: "/watch",
        port: "", protocol: "https:", search: "?v=dQw4w9WgXcQ"
    };
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
`

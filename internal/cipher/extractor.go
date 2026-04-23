package cipher

import (
	"fmt"
	"regexp"
	"strings"
)

// (legacySigHelperPattern moved to extractor_legacy.go — its only consumer.)

// sigCandidate represents a sig decryption function call with its control code.
type sigCandidate struct {
	funcName string
	literal  string // numeric control code passed as first arg
}

// preprocessPlayer extracts sig and n functions from player JS and generates
// standalone executable JS code. Returns code that when executed sets _result.n and _result.sig.
//
// Tries the "full player" approach first (include entire player.js with solver bindings),
// which handles YouTube's modern obfuscation. Falls back to legacy regex extraction
// for older player.js versions.
func preprocessPlayer(playerJS string) (string, error) {
	code, _, err := preprocessPlayerWithBranch(playerJS)
	return code, err
}

// preprocessPlayerWithBranch is preprocessPlayer, but it also reports which
// extraction branch succeeded ("full" or "legacy") so the caller can log it
// at the top of the compile path — otherwise a silent fallback to the legacy
// regex extractor looks identical to the full-player path in production logs.
func preprocessPlayerWithBranch(playerJS string) (code, branch string, err error) {
	if c, err := preprocessPlayerFull(playerJS); err == nil {
		return c, "full", nil
	}
	c, err := preprocessPlayerLegacy(playerJS)
	if err != nil {
		return "", "", err
	}
	return c, "legacy", nil
}

// --- Shared JS parsing helpers ---

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
			if i > len(js) {
				return len(js)
			}
			continue
		}
		if js[i] == quote {
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
			if i > len(js) {
				return len(js)
			}
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

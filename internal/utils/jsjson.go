package utils

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Port of yt-dlp's js_to_json (yt_dlp/utils/_utils.py). Go RE2 has no
// lookahead/lookbehind, so three constructs are emulated (all verified
// output-equivalent by the ported upstream test suite, see jsjson_test.go):
//   - the trailing-comma branch consumes ",<skip><bracket>" and re-emits the
//     skip minus comments plus the bracket (Python looks ahead);
//   - the decimal-object-key branch consumes "<digits><skip>:" and emits
//     `"N":` — whitespace between a decimal key and its colon is dropped;
//   - [eE]-led identifier runs and octal integers check the preceding source
//     byte in the handler instead of a lookbehind.
const (
	jsStrSingle = `'(?:\\.|[^\\'])*'`
	jsStrDouble = `"(?:\\.|[^\\"])*"`
	// Backtick string; not a raw literal because it contains backticks.
	jsStrTick = "`(?:\\\\.|[^\\\\`])*`"
	// Python: /\*(?:(?!\*/).)*?\*/ (negative-lookahead-guarded, stops at the
	// first */ even under backtracking pressure from an enclosing pattern).
	// RE2 has no lookahead, and a naive `.*?` is NOT equivalent here: when
	// this comment sub-pattern is embedded inside a larger alternative that
	// requires more input afterward (e.g. the trailing-comma branch's
	// `,` + jsSkip + `[\]\}]`), a non-matching short attempt forces `.*?` to
	// backtrack past the first `*/` and swallow everything up to a *later*
	// `*/`, corrupting unrelated content in between (verified against
	// yt-dlp test/test_utils.py's block-comment-in-array case). The
	// standard lookahead-free C-comment idiom below has no such backtrack
	// path: `\*+` followed by `/` is the only way out of the group, so the
	// match cannot extend past the first close.
	jsComment = `/\*[^*]*\*+(?:[^/*][^*]*\*+)*/|//[^\n]*\n`
	jsSkip    = `\s*(?:` + jsComment + `)?\s*`
)

var (
	jsStringRe = jsStrSingle + `|` + jsStrDouble + `|` + jsStrTick

	// Main dispatch: alternation order mirrors Python exactly (Go regexp is
	// leftmost-first, preserving preference order).
	jsMainRe = regexp.MustCompile(`(?s)` +
		`(?:` + jsStringRe + `)` +
		`|(?:` + jsComment + `)` +
		`|,` + jsSkip + `[\]\}]` +
		`|void\s0` +
		`|(?:[eE]|[a-df-zA-DF-Z_$])[.a-zA-Z_$0-9]*` +
		`|\b(?:0[xX][0-9a-fA-F]+|0+[0-7]+)(?:` + jsSkip + `:)?` +
		`|[0-9]+` + jsSkip + `:` +
		`|!+`)

	jsHexKeyRe   = regexp.MustCompile(`(?s)^(0[xX][0-9a-fA-F]+)` + jsSkip + `(:?)$`)
	jsOctKeyRe   = regexp.MustCompile(`(?s)^(0+[0-7]+)` + jsSkip + `(:?)$`)
	jsDecKeyRe   = regexp.MustCompile(`(?s)^([0-9]+)` + jsSkip + `:$`)
	jsCommentRe  = regexp.MustCompile(`(?s)` + jsComment)
	jsEscapeRe   = regexp.MustCompile(`(?s)(")|\\(.)`)
	jsTemplateRe = regexp.MustCompile(`(?s)\$\{([^}]+)\}`)

	jsArrayRe    = regexp.MustCompile(`(?:new\s+)?Array\((.*?)\)`)
	jsMapRe      = regexp.MustCompile(`new Map\((\[.*?\])?\)`)
	jsDateRe     = regexp.MustCompile(`new Date\((` + jsStringRe + `)\)`)
	jsCtorRe     = regexp.MustCompile(`new \w+\((.*?)\)`)
	jsParseIntRe = regexp.MustCompile(`parseInt\([^\d]+(\d+)[^\d]+\)`)
	jsIIFERe     = regexp.MustCompile(`\(function\([^)]*\)\s*\{[^}]*\}\s*\)\s*\(\s*(["'][^)]*["'])\s*\)`)
)

// JSToJSON converts a JavaScript object literal into strict JSON. vars maps
// identifier → replacement (a raw JSON value, or a bare string that will be
// quoted). strict mode errors on unknown identifiers instead of quoting them.
func JSToJSON(code string, vars map[string]string, strict bool) (string, error) {
	code = jsArrayRe.ReplaceAllString(code, `[$1]`)
	var mapErr error
	code = jsMapRe.ReplaceAllStringFunc(code, func(m string) string {
		inner := jsMapRe.FindStringSubmatch(m)[1]
		if inner == "" {
			inner = "[]"
		}
		out, err := jsCreateMap(inner, vars, strict)
		if err != nil {
			if mapErr == nil {
				mapErr = err
			}
			return m
		}
		return out
	})
	if mapErr != nil {
		return "", mapErr
	}
	if !strict {
		code = jsDateRe.ReplaceAllString(code, `$1`)
		code = jsCtorRe.ReplaceAllStringFunc(code, func(m string) string {
			b, _ := json.Marshal(m)
			return string(b)
		})
		code = jsParseIntRe.ReplaceAllString(code, `$1`)
		code = jsIIFERe.ReplaceAllString(code, `$1`)
	}

	var sb strings.Builder
	last := 0
	for _, loc := range jsMainRe.FindAllStringIndex(code, -1) {
		start, end := loc[0], loc[1]
		sb.WriteString(code[last:start])
		repl, err := jsFixKV(code, start, code[start:end], vars, strict)
		if err != nil {
			return "", err
		}
		sb.WriteString(repl)
		last = end
	}
	sb.WriteString(code[last:])
	return sb.String(), nil
}

// jsCreateMap converts the array-of-pairs argument of `new Map(...)` into a
// JSON object, preserving insertion order (mirrors Python dict + json.dumps).
func jsCreateMap(inner string, vars map[string]string, strict bool) (string, error) {
	converted, err := JSToJSON(inner, vars, strict)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(strings.NewReader(converted))
	dec.UseNumber()
	var pairs [][]any
	if err := dec.Decode(&pairs); err != nil {
		return "", fmt.Errorf("js_to_json: new Map: %w", err)
	}
	seen := make(map[string]int, len(pairs)) // key → index in out
	type kv struct {
		k string
		v any
	}
	out := make([]kv, 0, len(pairs))
	for _, p := range pairs {
		if len(p) != 2 {
			return "", fmt.Errorf("js_to_json: new Map: pair of length %d", len(p))
		}
		key := jsMapKeyString(p[0])
		if i, ok := seen[key]; ok {
			out[i].v = p[1]
			continue
		}
		seen[key] = len(out)
		out = append(out, kv{key, p[1]})
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, e := range out {
		if i > 0 {
			sb.WriteString(", ")
		}
		kb, _ := json.Marshal(e.k)
		vb, err := json.Marshal(e.v)
		if err != nil {
			return "", fmt.Errorf("js_to_json: new Map value: %w", err)
		}
		sb.Write(kb)
		sb.WriteString(": ")
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return sb.String(), nil
}

// jsMapKeyString ports Python's json.dumps dict-key coercion (json/encoder.py
// _iterencode_dict): str keys pass through as-is, and the non-string JSON
// scalars that can appear as a decoded Map-pair key get Python's fixed
// string forms — notably None → "null", not Go's fmt-default "<nil>".
// json.Number (int/float) round-trips through its original source text,
// matching Python's int()/float repr for the common case where the source
// text is unchanged by decoding.
func jsMapKeyString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// jsFixKV is the port of Python's fix_kv closure. code+start give access to
// the byte preceding the match for the lookbehind emulations.
func jsFixKV(code string, start int, v string, vars map[string]string, strict bool) (string, error) {
	switch v {
	case "true", "false", "null":
		return v, nil
	case "undefined", "void 0":
		return "null", nil
	}
	if strings.HasPrefix(v, "/*") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "!") {
		return "", nil
	}
	if v[0] == ',' {
		// Trailing-comma branch: ",<skip><bracket>" — drop the comma and any
		// comments, keep whitespace + bracket (see divergence note above).
		return jsCommentRe.ReplaceAllString(v[1:], ""), nil
	}
	if c := v[0]; c == '\'' || c == '"' || c == '`' {
		body := v[1 : len(v)-1]
		if c == '`' {
			var tmplErr error
			body = jsTemplateRe.ReplaceAllStringFunc(body, func(m string) string {
				expr := jsTemplateRe.FindStringSubmatch(m)[1]
				evaluated, err := JSToJSON(expr, vars, strict)
				if err != nil {
					if tmplErr == nil {
						tmplErr = err
					}
					return m
				}
				if evaluated != "" && evaluated[0] == '"' {
					var s string
					if json.Unmarshal([]byte(evaluated), &s) == nil {
						return s
					}
				}
				return evaluated
			})
			if tmplErr != nil {
				return "", tmplErr
			}
		}
		escaped := jsEscapeRe.ReplaceAllStringFunc(body, jsProcessEscape)
		return `"` + escaped + `"`, nil
	}
	if m := jsHexKeyRe.FindStringSubmatch(v); m != nil {
		n, err := parseJSInt(m[1][2:], 16)
		if err != nil {
			return "", fmt.Errorf("js_to_json: hex %q: %w", m[1], err)
		}
		return jsIntOut(n, m[2] == ":"), nil
	}
	if m := jsOctKeyRe.FindStringSubmatch(v); m != nil {
		n, err := parseJSInt(m[1], 8)
		if err != nil {
			return "", fmt.Errorf("js_to_json: octal %q: %w", m[1], err)
		}
		if start > 0 && code[start-1] == '.' {
			if m[2] != ":" {
				// Python (?<!\.): the outer octal alternative doesn't match
				// at all here, and the decimal-key alternative's lookahead
				// for a trailing ":" also fails with nothing to look ahead
				// at — so Python's whole regex leaves these digits entirely
				// unmatched (no conversion).
				return v, nil
			}
			// Python (?<!\.): the outer octal alternative doesn't match, but
			// the decimal-key alternative's lookahead still finds the ":"
			// and matches just the digits (its match never includes the
			// skip+":" — that's a lookahead, not a consumption). fix_kv then
			// re-tests those bare digits against INTEGER_TABLE, which has no
			// dot guard, and still recognizes the octal shape — so the
			// *value* stays base-8 — but since Python's match never included
			// skip+":", it was never folded into a quoted `"N":` key; it
			// stays raw, unmatched source text. Go's regex has no lookbehind
			// so it greedily consumed skip+":" as part of this match;
			// splice it back in unconverted instead of producing a key form.
			return n + v[len(m[1]):], nil
		}
		return jsIntOut(n, m[2] == ":"), nil
	}
	if m := jsDecKeyRe.FindStringSubmatch(v); m != nil {
		return `"` + m[1] + `":`, nil
	}
	if (v[0] == 'e' || v[0] == 'E') && start > 0 && code[start-1] >= '0' && code[start-1] <= '9' {
		return v, nil // Python (?<![0-9])[eE]: exponent of a numeric literal
	}
	if val, ok := vars[v]; ok {
		if !strict && !json.Valid([]byte(val)) {
			b, _ := json.Marshal(val)
			return string(b), nil
		}
		return val, nil
	}
	if !strict {
		b, _ := json.Marshal(v)
		return string(b), nil
	}
	return "", fmt.Errorf("js_to_json: unknown value: %s", v)
}

// parseJSInt mirrors Python's int(text, base): arbitrary precision, so a
// hex/octal literal of any width converts to its exact decimal digits and
// never errors on magnitude alone (only on malformed digits, which the
// caller's regex already excludes). The fast path handles the overwhelming
// common case — literals that fit in 64 bits — at strconv cost; math/big
// only engages for oversized literals like 0xFFFFFFFFFFFFFFFFF that exceed
// even uint64's range.
func parseJSInt(digits string, base int) (string, error) {
	if n, err := strconv.ParseUint(digits, base, 64); err == nil {
		return strconv.FormatUint(n, 10), nil
	}
	bi, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return "", fmt.Errorf("invalid integer literal %q (base %d)", digits, base)
	}
	return bi.String(), nil
}

func jsIntOut(n string, isKey bool) string {
	if isKey {
		return `"` + n + `":`
	}
	return n
}

// jsProcessEscape ports Python's process_escape: m is either a bare `"` or a
// two-character escape sequence `\X`.
func jsProcessEscape(m string) string {
	if m == `"` {
		return `\"`
	}
	esc := m[1:]
	if len(esc) == 1 && strings.ContainsAny(esc, "\"\\bfnrtu") {
		return `\` + esc
	}
	if esc == "x" {
		return `\u00` // \xAB → «
	}
	if esc == "\n" {
		return "" // line continuation
	}
	return esc
}

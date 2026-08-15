# Attestation-Challenge POT Coherence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GVS (segment-URL) PO tokens are minted by a fresh BotGuard minter built from the job's own watch-page `window.ytAtN` attestation challenge, bound to the video ID, with provenance logging — so POT-enforced media (premieres) stops 403-ing our segment fetches.

**Architecture:** Watch-page fetch extracts the `bgChallenge` JSON (via a full Go port of yt-dlp's `js_to_json`), carries it on `VideoInfo`, and the three YouTube download strategies mint through a new `PotProvider.GenerateGvsPoToken` that forces a fresh sidecar minter built from that challenge. Spec: `docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md`.

**Tech Stack:** Go 1.26 (no CGo), Node sidecar (`bgutil-sidecar/src/server.js`, JSON-RPC over stdio), bgutils-js.

## Global Constraints

- No CGo; pure Go only. Windows is the primary platform (paths in code use `filepath`).
- The worker/db/youtube logger pattern is an **anonymous** 4-method interface repeated per struct — do not extract a shared named interface (CLAUDE.md). `engine.DownloaderLogger` is already named; leave it.
- Sidecar JS changes require `cd bgutil-sidecar && node build.mjs` before `go build` picks them up (regenerates `internal/bgutils/embed/` tarball).
- Sidecar tests skip automatically when embed blobs are missing; live-network tests are env-gated (`MOOMBOX_LIVE_BG_TEST=1`).
- Every commit message ends with:
  ```
  Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01R7cRqJNFF17EW9dNZ9af5Z
  ```
- Run `go build ./... && go vet ./...` before each commit; the named test command(s) must pass.

---

### Task 1: `JSToJSON` — Go port of yt-dlp's `js_to_json`

**Files:**
- Create: `internal/utils/jsjson.go`
- Test: `internal/utils/jsjson_test.go`

**Interfaces:**
- Produces: `utils.JSToJSON(code string, vars map[string]string, strict bool) (string, error)` — converts a JS object literal to strict JSON. Non-strict mode never errors except for malformed `new Map(...)` payloads; strict mode errors on unknown identifiers.

Reference implementation being ported: `references/yt-dlp/yt_dlp/utils/_utils.py:2776-2855` (`js_to_json`). Known, documented divergences (RE2 has no lookahead/lookbehind, so those guards are emulated or consume-and-re-emit):

1. Trailing-comma branch consumes `,<skip><bracket>` and re-emits skip-minus-comments + bracket (Python looks ahead). Output-identical.
2. Decimal-key branch consumes `<digits><skip>:` and emits `"N":` — whitespace/comments between a decimal key and its colon are dropped (Python preserves the whitespace). No behavior change after `json.Unmarshal`.
3. `[eE]`-led runs and octal literals check the preceding source byte in the handler instead of lookbehind.

- [ ] **Step 1: Write the failing test**

`internal/utils/jsjson_test.go` — table-driven, cases transcribed from yt-dlp `test/test_utils.py:1063-1318`. Two comparison modes: `exact` (string equality on output) and `loads` (unmarshal both sides, compare with `reflect.DeepEqual`).

```go
package utils

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
	return v
}

func TestJSToJSONExact(t *testing.T) {
	// Cases where yt-dlp asserts the exact output string.
	cases := []struct{ name, in, want string }{
		{"single_quotes", "{\n            'clip':{'provider':'pseudo'}\n        }", "{\n            \"clip\":{\"provider\":\"pseudo\"}\n        }"},
		{"nested_null", "{\n            'playlist':[{'controls':{'all':null}}]\n        }", "{\n            \"playlist\":[{\"controls\":{\"all\":null}}]\n        }"},
		{"escaped_quotes", `"The CW\'s \'Crazy Ex-Girlfriend\'"`, `"The CW's 'Crazy Ex-Girlfriend'"`},
		{"numeric_keys_trailing_comma", "{\n            0:{src:'skipped', type: 'application/dash+xml'},\n            1:{src:'skipped', type: 'application/vnd.apple.mpegURL'},\n        }", "{\n            \"0\":{\"src\":\"skipped\", \"type\": \"application/dash+xml\"},\n            \"1\":{\"src\":\"skipped\", \"type\": \"application/vnd.apple.mpegURL\"}\n        }"},
		{"already_json", `{"foo":101}`, `{"foo":101}`},
		{"duration_string", `{"duration": "00:01:07"}`, `{"duration": "00:01:07"}`},
		{"scientific_notation", `{segments: [{"offset":-3.885780586188048e-16,"duration":39.75000000000001}]}`, `{"segments": [{"offset":-3.885780586188048e-16,"duration":39.75000000000001}]}`},
		{"malformed_42a1", `42a1`, `42"a1"`},
		{"malformed_42a-1", `42a-1`, `42"a"-1`},
		{"template_iife", "{a: `${e(\"\")}`}", `{"a": "\"e\"(\"\")"}`},
		{"template_var", "`Hello ${name}`", `"Hello world"`}, // vars: name -> "world" (set below)
		{"template_var_twice", "`${name}${name}`", `"XX"`},   // vars: name -> "X"
		{"template_num_twice", "`${name}${name}`", `"55"`},   // vars: name -> 5
		{"template_num_quoted", "`${name}\"${name}\"`", `"5\"5\""`}, // vars: name -> 5
		{"template_unresolved", "`${name}`", `"name"`}, // no vars
	}
	vars := map[string]map[string]string{
		"template_var":        {"name": `"world"`},
		"template_var_twice":  {"name": `"X"`},
		"template_num_twice":  {"name": `5`},
		"template_num_quoted": {"name": `5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSToJSON(tc.in, vars[tc.name], false)
			if err != nil {
				t.Fatalf("JSToJSON error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJSToJSONLoads(t *testing.T) {
	// Cases where yt-dlp asserts json.loads equality only.
	cases := []struct{ name, in, want string }{
		{"escapes_edge", `{abc_def:'1\'\\2\\\'3"4'}`, `{"abc_def": "1'\\2\\'3\"4"}`},
		{"plain_bool", `{"abc": true}`, `{"abc": true}`},
		{"trailing_comma_array", `["abc", "def",]`, `["abc","def"]`},
		{"block_comments_array", "[/*comment\n*/\"abc\"/*comment\n*/,/*comment\n*/\"def\",/*comment\n*/]", `["abc","def"]`},
		{"line_comments_array", "[//comment\n\"abc\" //comment\n,//comment\n\"def\",//comment\n]", `["abc","def"]`},
		{"trailing_comma_obj", `{"abc": "def",}`, `{"abc":"def"}`},
		{"block_comments_obj", "{/*comment\n*/\"abc\"/*comment\n*/:/*comment\n*/\"def\"/*comment\n*/,/*comment\n*/}", `{"abc":"def"}`},
		{"tricky_comment_key", "{ 0: /* \" \n */ \",]\" , }", `{"0": ",]"}`},
		{"comment_wrapped_key", "{ /*comment\n*/0/*comment\n*/: /* \" \n */ \",]\" , }", `{"0": ",]"}`},
		{"line_comment_value", "{ 0: // comment\n1 }", `{"0": 1}`},
		{"solidus_escape", `["<p>x<\/p>"]`, `["<p>x</p>"]`},
		{"hex_escape", `["\xaa"]`, `["ª"]`},
		{"line_continuation", "['a\\\nb']", `["ab"]`},
		{"comments_everywhere", "/*comment\n*/[/*comment\n*/'a\\\nb'/*comment\n*/]/*comment\n*/", `["ab"]`},
		{"hex_key_value", `{0xff:0xff}`, `{"255": 255}`},
		{"hex_with_comments", "{/*comment\n*/0xff/*comment\n*/:/*comment\n*/0xff/*comment\n*/}", `{"255": 255}`},
		{"octal_key_value", `{077:077}`, `{"63": 63}`},
		{"octal_with_comments", "{/*comment\n*/077/*comment\n*/:/*comment\n*/077/*comment\n*/}", `{"63": 63}`},
		{"decimal_key_value", `{42:42}`, `{"42": 42}`},
		{"decimal_with_comments", "{/*comment\n*/42/*comment\n*/:/*comment\n*/42/*comment\n*/}", `{"42": 42}`},
		{"decimal_key_sci_value", `{42:4.2e1}`, `{"42": 42.0}`},
		{"hex_lookalike_strings", `{ "0x40": "0x40" }`, `{"0x40": "0x40"}`},
		{"octal_lookalike_strings", `{ "040": "040" }`, `{"040": "040"}`},
		{"comment_containing_braces", "[1,//{},\n2]", `[1,2]`},
		{"unnecessary_escapes", `"\^\$\#"`, `"^$#"`},
		{"quote_escape_normalize", "'\"\\\"\"'", `"\"\"\""`},
		{"date_and_paren_string", `[new Date("spam"), '("eggs")']`, `["spam", "(\"eggs\")"]`},
		{"float_zeroes", `[0.077, 7.06, 29.064, 169.0072]`, `[0.077, 7.06, 29.064, 169.0072]`},
		{"map_ctor", `new Map([["a", 5]])`, `{"a": 5}`},
		{"array_ctor_bare", `Array(5, 10)`, `[5, 10]`},
		{"array_ctor_new", `new Array(15,5)`, `[15, 5]`},
		{"map_of_arrays", `new Map([Array(5, 10),new Array(15,5)])`, `{"5": 10, "15": 5}`},
		{"date_ctor_double", `new Date("123")`, `"123"`},
		{"date_ctor_single", `new Date('2023-10-19')`, `"2023-10-19"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSToJSON(tc.in, nil, false)
			if err != nil {
				t.Fatalf("JSToJSON error: %v", err)
			}
			if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, tc.want)) {
				t.Errorf("loads mismatch:\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

func TestJSToJSONBangPrefix(t *testing.T) {
	got, err := JSToJSON("{\n            a: !0,\n            b: !1,\n            c: !!0,\n            d: !!42.42,\n            e: !!![],\n            f: !\"abc\",\n            g: !\"\",\n            !42: 42\n        }", nil, false)
	if err != nil {
		t.Fatalf("JSToJSON error: %v", err)
	}
	want := `{"a": 0, "b": 1, "c": 0, "d": 42.42, "e": [], "f": "abc", "g": "", "42": 42}`
	if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("loads mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestJSToJSONVars(t *testing.T) {
	got, err := JSToJSON("{\n'null': a,\n'nullStr': b,\n'true': c,\n'trueStr': d,\n'false': e,\n'falseStr': f,\n'unresolvedVar': g,\n}",
		map[string]string{"a": "null", "b": `"null"`, "c": "true", "d": `"true"`, "e": "false", "f": `"false"`, "g": "var"}, false)
	if err != nil {
		t.Fatalf("JSToJSON error: %v", err)
	}
	want := `{"null": null, "nullStr": "null", "true": true, "trueStr": "true", "false": false, "falseStr": "false", "unresolvedVar": "var"}`
	if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("loads mismatch:\n got  %s\n want %s", got, want)
	}

	got, err = JSToJSON("{\n'int': a,\n'intStr': b,\n'float': c,\n'floatStr': d,\n}",
		map[string]string{"a": "123", "b": `"123"`, "c": "1.23", "d": `"1.23"`}, false)
	if err != nil {
		t.Fatalf("JSToJSON error: %v", err)
	}
	want = `{"int": 123, "intStr": "123", "float": 1.23, "floatStr": "1.23"}`
	if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("loads mismatch:\n got  %s\n want %s", got, want)
	}

	got, err = JSToJSON("{\n'object': a,\n'objectStr': b,\n'array': c,\n'arrayStr': d,\n}",
		map[string]string{"a": "{}", "b": `"{}"`, "c": "[]", "d": `"[]"`}, false)
	if err != nil {
		t.Fatalf("JSToJSON error: %v", err)
	}
	want = `{"object": {}, "objectStr": "{}", "array": [], "arrayStr": "[]"}`
	if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("loads mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestJSToJSONStrict(t *testing.T) {
	if _, err := JSToJSON(`{a: unknownVar}`, nil, true); err == nil {
		t.Fatal("strict mode should error on unknown identifier")
	}
	// strict + vars returns raw value without validity check
	got, err := JSToJSON(`{'k': a}`, map[string]string{"a": "123"}, true)
	if err != nil {
		t.Fatalf("JSToJSON error: %v", err)
	}
	if !reflect.DeepEqual(mustJSON(t, got), mustJSON(t, `{"k": 123}`)) {
		t.Errorf("got %s", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/utils/ -run TestJSToJSON -v`
Expected: FAIL (compile error, `JSToJSON` undefined)

- [ ] **Step 3: Write the implementation**

`internal/utils/jsjson.go`:

```go
package utils

import (
	"encoding/json"
	"fmt"
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
	// Python: /\*(?:(?!\*/).)*?\*/ — non-greedy to the first */ is equivalent.
	jsComment = `/\*.*?\*/|//[^\n]*\n`
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
		key := fmt.Sprint(p[0]) // json.Number and string both print verbatim
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
		n, err := strconv.ParseInt(m[1][2:], 16, 64)
		if err != nil {
			return "", fmt.Errorf("js_to_json: hex %q: %w", m[1], err)
		}
		return jsIntOut(n, m[2] == ":"), nil
	}
	if m := jsOctKeyRe.FindStringSubmatch(v); m != nil {
		if start > 0 && code[start-1] == '.' {
			return v, nil // Python (?<!\.): fraction digits, not an octal
		}
		n, err := strconv.ParseInt(m[1], 8, 64)
		if err != nil {
			return "", fmt.Errorf("js_to_json: octal %q: %w", m[1], err)
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

func jsIntOut(n int64, isKey bool) string {
	if isKey {
		return `"` + strconv.FormatInt(n, 10) + `":`
	}
	return strconv.FormatInt(n, 10)
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/utils/ -run TestJSToJSON -v`
Expected: PASS (all subtests). If an exact-mode case fails on whitespace only, check divergence notes 1–2 first — the fix belongs in the consuming-branch handlers, not the test.

- [ ] **Step 5: Commit**

```bash
git add internal/utils/jsjson.go internal/utils/jsjson_test.go
git commit -m "feat(utils): port yt-dlp js_to_json"
```

---

### Task 2: Attestation-challenge extraction from the watch page

**Files:**
- Modify: `internal/youtube/watch_page.go` (struct at :55, function at :68)
- Test: `internal/youtube/watch_page_test.go` (append)

**Interfaces:**
- Consumes: `utils.JSToJSON` (Task 1).
- Produces: `WatchPageResult.AttestationChallenge string` (compact `bgChallenge` JSON, `""` when unavailable) and package-private `extractAttestationChallenge(html string) string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/youtube/watch_page_test.go`:

```go
func TestExtractAttestationChallenge(t *testing.T) {
	// Shape per moonarchive 96344fe: window.ytAtN({...}) whose R key is a
	// JSON *string* containing bgChallenge.
	challenge := `{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/th/abc.js"},"interpreterHash":"h","program":"prog","globalName":"trayride"}`
	rPayload, _ := json.Marshal(map[string]any{"bgChallenge": json.RawMessage(challenge)})
	atn, _ := json.Marshal(string(rPayload))
	page := `<html><script>window.ytAtN({R: ` + string(atn) + `, other: 1});</script></html>`

	got := extractAttestationChallenge(page)
	if got == "" {
		t.Fatal("expected challenge, got empty")
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if back["globalName"] != "trayride" || back["program"] != "prog" {
		t.Errorf("challenge content mangled: %s", got)
	}

	for name, html := range map[string]string{
		"absent":            `<html><script>var x = 1;</script></html>`,
		"malformed_js":      `<html><script>window.ytAtN({R: });</script></html>`,
		"missing_R":         `<html><script>window.ytAtN({Q: "{}"});</script></html>`,
		"R_not_json":        `<html><script>window.ytAtN({R: "not json"});</script></html>`,
		"missing_challenge": `<html><script>window.ytAtN({R: "{\"noChallenge\":1}"});</script></html>`,
	} {
		if got := extractAttestationChallenge(html); got != "" {
			t.Errorf("%s: expected empty, got %q", name, got)
		}
	}
}
```

(Add `"encoding/json"` to the test file imports if not present.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/youtube/ -run TestExtractAttestationChallenge -v`
Expected: FAIL (compile error, `extractAttestationChallenge` undefined)

- [ ] **Step 3: Implement**

In `internal/youtube/watch_page.go`:

1. Add to `WatchPageResult` (after `ChatErr error`):

```go
	// AttestationChallenge is the compact JSON of the BotGuard bgChallenge
	// YouTube embedded in this page load via window.ytAtN(...) — the
	// session's own attestation challenge, used to mint session-coherent
	// GVS PO tokens (moonarchive 96344fe parity). Empty when the page did
	// not carry one or it failed to parse; consumers must treat empty as
	// "fall back to the sidecar's /att/get flow".
	AttestationChallenge string
```

2. Add the extractor (bottom of file) and populate it in `FetchWatchPage` alongside the existing `extractYtcfgAndPlayerResponse(html)` call — `result.AttestationChallenge = extractAttestationChallenge(html)`:

```go
// ytAtNRe captures the JS object literal passed to window.ytAtN(...).
// Mirrors moonarchive's INITIAL_ATTESTATION_PATTERN; RE2-safe (no
// lookarounds), non-greedy so it stops at the first plausible close.
var ytAtNRe = regexp.MustCompile(`window\.ytAtN\(\s*(\{[\s\S]*?\})\s*\)`)

// extractAttestationChallenge pulls the BotGuard bgChallenge out of a watch
// page's window.ytAtN(...) blob. The blob is a JS object literal whose "R"
// key holds a JSON string; inside that is bgChallenge. Returns compact JSON
// of bgChallenge, or "" on any miss/parse failure — absence is a normal
// result (the POT sidecar falls back to /att/get), never an error.
func extractAttestationChallenge(html string) string {
	m := ytAtNRe.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	jsonStr, err := utils.JSToJSON(m[1], nil, false)
	if err != nil {
		return ""
	}
	var outer map[string]any
	if json.Unmarshal([]byte(jsonStr), &outer) != nil {
		return ""
	}
	rStr, ok := outer["R"].(string)
	if !ok {
		return ""
	}
	var r struct {
		BgChallenge json.RawMessage `json:"bgChallenge"`
	}
	if json.Unmarshal([]byte(rStr), &r) != nil || len(r.BgChallenge) == 0 {
		return ""
	}
	return string(r.BgChallenge)
}
```

Add imports `encoding/json`, `regexp`, and `github.com/vampiricwulf/Moombox/internal/utils` as needed (utils is likely already imported for `FetchBody`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/youtube/ -run TestExtractAttestationChallenge -v && go test ./internal/youtube/`
Expected: PASS, and no regressions in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/youtube/watch_page.go internal/youtube/watch_page_test.go
git commit -m "feat(youtube): extract ytAtN attestation challenge from watch page"
```

---

### Task 3: Carry the challenge on `VideoInfo`

**Files:**
- Modify: `internal/youtube/types.go:32-64` (VideoInfo struct)
- Modify: `internal/youtube/player_api_strategy.go` (every return path of `GetVideoInfoAuthenticated` :61-237 and `GetVideoInfoPublic` :240-324)

**Interfaces:**
- Produces: `VideoInfo.AttestationChallenge string` — set on every VideoInfo either function returns; empty when the watch page had none.

- [ ] **Step 1: Add the field**

In `types.go` after `PlayabilityReason`:

```go
	// AttestationChallenge is the session's watch-page BotGuard challenge
	// (see WatchPageResult.AttestationChallenge). Rides on VideoInfo so
	// download strategies can mint session-coherent GVS PO tokens.
	AttestationChallenge string `json:"-"`
```

- [ ] **Step 2: Add the carrier helper and apply to every return**

In `player_api_strategy.go`:

```go
// withAttestation stamps the watch page's attestation challenge onto the
// VideoInfo being returned. Applied at every GetVideoInfo* return site
// explicitly — NOT via mergeWatchPageMetadata, which several early returns
// skip or call with a nil source.
func withAttestation(info *VideoInfo, wp *WatchPageResult) *VideoInfo {
	if info != nil && wp != nil {
		info.AttestationChallenge = wp.AttestationChallenge
	}
	return info
}
```

Wrap every `return <info>, nil` in both functions, including inside `finalizeVideoInfo` callers:

- `GetVideoInfoAuthenticated`: returns at :184 (`embResult`), :217 (`vrResult`), :225 (`wpParsed`), :233 (`wcResult`), :236 (`finalizeVideoInfo(...)`).
- `GetVideoInfoPublic`: returns at :263 (`wpParsed`), :280 (`embResult`), :293 (`vrResult`), :299 (`wpParsed`), :323 (`finalizeVideoInfo(...)`).

Pattern: `return withAttestation(embResult, wp), nil` (authenticated uses `wp`; public uses `wp` too — both functions already hold the `*WatchPageResult` in scope). The `GetVideoInfoPublic` early-error return at :265 (`return nil, err`) stays as-is.

Also add the spec §4.2 Debug line in BOTH functions, right after the watch-page fetch succeeds (`extractAttestationChallenge` itself has no logger):

```go
	if wp.AttestationChallenge == "" {
		p.logger.Debug("[PlayerApi] watch page carried no attestation challenge", "videoID", videoID)
	}
```

(Place it after the `wp` nil-guard/fallback assignment so `wp` is always non-nil at that point.)

- [ ] **Step 3: Build and test the package**

Run: `go build ./... && go test ./internal/youtube/`
Expected: PASS. (No new test — the helper is a two-line assignment exercised by Task 7's end-to-end path; the extraction logic itself was tested in Task 2.)

- [ ] **Step 4: Commit**

```bash
git add internal/youtube/types.go internal/youtube/player_api_strategy.go
git commit -m "feat(youtube): carry attestation challenge on VideoInfo"
```

---

### Task 4: Sidecar — challenge-sourced fresh minters + provenance

**Files:**
- Modify: `bgutil-sidecar/src/server.js` (`generateMinter` :96-180, `getOrCreateMinter` :189-217, `generatePoToken` :219-233, dispatch case :251-261)

**Interfaces:**
- Consumes: RPC params `{binding: string, challenge?: string, freshMinter?: bool}` (challenge = compact bgChallenge JSON from Task 2).
- Produces: RPC result `{poToken, binding, expiresAt, minterSource: "challenge"|"att_get", minterFresh: bool}`. Legacy calls (`{binding}` only) behave exactly as today and report `minterSource`/`minterFresh` truthfully.

- [ ] **Step 1: Rework `generateMinter` to accept a challenge**

Replace the function signature and step 1; steps 2–6 are unchanged except they read from the `challenge` variable and the return gains `minterSource`:

```js
// challenge: a parsed bgChallenge object from the caller's watch page, or
// null → fetch our own from /att/get (the legacy, session-incoherent path).
async function generateMinter(challenge) {
    let minterSource = "challenge";
    if (!challenge) {
        minterSource = "att_get";
        // 1. Fetch the BotGuard challenge from YouTube's /att/get endpoint.
        const attResp = await fetch(ATT_GET_URL, {
            /* ... existing fetch options unchanged ... */
        });
        if (!attResp.ok) {
            throw new Error(`att/get HTTP ${attResp.status}`);
        }
        const attBody = await attResp.json();
        challenge = attBody && attBody.bgChallenge;
        if (!challenge) {
            throw new Error("att/get response missing bgChallenge");
        }
    }
    // ... steps 2-6 exactly as before, operating on `challenge` ...
    return {
        minter,
        expiresAt: Date.now() + estimatedTtlSecs * 1000,
        webPoSignalOutput,
        globalName: challenge.globalName,
        minterSource,
    };
}
```

- [ ] **Step 2: Rework `getOrCreateMinter` for freshMinter + provenance**

```js
// freshMinter forces a regeneration even when the cache is valid (the
// fresh-per-GVS-mint policy). Concurrent regens share one in-flight
// generateMinter via minterPromise — a joiner may therefore get a minter
// built from ANOTHER request's challenge; same session, acceptable.
async function getOrCreateMinter(challenge, freshMinter) {
    if (!freshMinter && cachedMinter && Date.now() < cachedMinter.expiresAt) {
        return { m: cachedMinter, fresh: false };
    }
    if (!minterPromise) {
        minterPromise = generateMinter(challenge).finally(() => {
            minterPromise = null;
        });
    }
    const prev = cachedMinter;
    cachedMinter = await minterPromise;
    if (prev && prev.globalName && prev.globalName !== cachedMinter.globalName) {
        try {
            delete globalThis[prev.globalName];
        } catch (e) {
            logWarn(`could not free stale interpreter global: ${e?.message ?? e}`);
        }
    }
    stats.cachedMinters = 1;
    return { m: cachedMinter, fresh: true };
}
```

- [ ] **Step 3: Rework `generatePoToken` and the dispatch case**

```js
async function generatePoToken(binding, challengeJSON, freshMinter) {
    if (!binding || typeof binding !== "string") {
        throw new Error("missing or invalid binding");
    }
    let challenge = null;
    if (challengeJSON && typeof challengeJSON === "string") {
        try {
            const parsed = JSON.parse(challengeJSON);
            // Defensive shape check: a challenge missing its program or
            // interpreter URL would crash generateMinter mid-flight; treat
            // it as absent so the /att/get fallback runs instead.
            if (parsed && parsed.program && parsed.interpreterUrl) {
                challenge = parsed;
            } else {
                logWarn("challenge missing program/interpreterUrl; using /att/get");
            }
        } catch (e) {
            logWarn(`invalid challenge JSON ignored: ${e?.message ?? e}`);
        }
    }
    const { m, fresh } = await getOrCreateMinter(challenge, !!freshMinter);
    const poToken = await m.minter.mintAsWebsafeString(binding);
    if (!poToken) {
        throw new Error("WebPoMinter returned empty poToken");
    }
    return {
        poToken,
        binding,
        expiresAt: m.expiresAt,
        minterSource: m.minterSource,
        minterFresh: fresh,
    };
}
```

Dispatch case becomes: `const result = await generatePoToken(params.binding, params.challenge, params.freshMinter);`

- [ ] **Step 4: Rebuild the sidecar bundle and run existing sidecar tests**

```bash
cd bgutil-sidecar && node build.mjs && cd ..
go build ./... && go test ./internal/bgutils/... -count=1
```

Expected: build OK, existing sidecar tests PASS (legacy `{binding}`-only calls hit `getOrCreateMinter(null, false)` — identical behavior; live mint tests only run with `MOOMBOX_LIVE_BG_TEST=1`).

- [ ] **Step 5: Commit**

```bash
git add bgutil-sidecar/src/server.js internal/bgutils/embed/
git commit -m "feat(sidecar): challenge-sourced fresh minters with mint provenance"
```

(If `internal/bgutils/embed/` blobs are gitignored, commit only `server.js` — check `git status` output.)

---

### Task 5: Go sidecar client — `GenerateGvsPoToken`

**Files:**
- Modify: `internal/bgutils/sidecar/sidecar.go` (after `GeneratePoToken` :370-381)
- Test: `internal/bgutils/sidecar/sidecar_live_test.go` (append, env-gated)

**Interfaces:**
- Consumes: Task 4's RPC contract.
- Produces:
  ```go
  type GvsMintResult struct {
      PoToken      string `json:"poToken"`
      MinterSource string `json:"minterSource"` // "challenge" | "att_get"
      MinterFresh  bool   `json:"minterFresh"`
  }
  func (s *Sidecar) GenerateGvsPoToken(ctx context.Context, binding, challenge string) (GvsMintResult, error)
  ```

- [ ] **Step 1: Implement**

```go
// GvsMintResult carries a minted GVS PO token plus the provenance fields the
// worker's "[POT] GVS mint" log line reports (spec §4.6): which challenge
// source built the minter and whether it was regenerated for this mint.
type GvsMintResult struct {
	PoToken      string `json:"poToken"`
	MinterSource string `json:"minterSource"` // "challenge" | "att_get"
	MinterFresh  bool   `json:"minterFresh"`
}

// GenerateGvsPoToken mints a GVS (segment-URL) PO token with the
// fresh-minter-per-mint policy: the sidecar regenerates its BotGuard minter
// for this call, building it from the supplied watch-page challenge when
// non-empty (session-coherent) or its own /att/get fetch otherwise.
func (s *Sidecar) GenerateGvsPoToken(ctx context.Context, binding, challenge string) (GvsMintResult, error) {
	params := map[string]any{"binding": binding, "freshMinter": true}
	if challenge != "" {
		params["challenge"] = challenge
	}
	var result GvsMintResult
	if err := s.call(ctx, "generatePoToken", params, &result); err != nil {
		return GvsMintResult{}, err
	}
	if result.PoToken == "" {
		return GvsMintResult{}, errors.New("sidecar returned empty poToken")
	}
	return result, nil
}
```

- [ ] **Step 2: Add the env-gated live test**

Append to `sidecar_live_test.go` (mirror the file's existing gating pattern — it checks `MOOMBOX_LIVE_BG_TEST`):

```go
// TestSidecarLiveGvsMint exercises the freshMinter + provenance path against
// real YouTube endpoints. No page challenge is supplied (none is available in
// a test), so provenance must report the /att/get source and a fresh minter.
func TestSidecarLiveGvsMint(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") == "" {
		t.Skip("set MOOMBOX_LIVE_BG_TEST=1 to run live BotGuard tests")
	}
	s := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := s.GenerateGvsPoToken(ctx, "dQw4w9WgXcQ", "")
	if err != nil {
		t.Fatalf("GenerateGvsPoToken: %v", err)
	}
	if res.PoToken == "" || res.MinterSource != "att_get" || !res.MinterFresh {
		t.Errorf("unexpected result: %+v", res)
	}
	// Second fresh mint must regenerate again (freshMinter honored).
	res2, err := s.GenerateGvsPoToken(ctx, "dQw4w9WgXcQ", "")
	if err != nil {
		t.Fatalf("second GenerateGvsPoToken: %v", err)
	}
	if !res2.MinterFresh {
		t.Error("second GVS mint reused the cached minter; freshMinter not honored")
	}
}
```

- [ ] **Step 3: Build + run**

Run: `go build ./... && go test ./internal/bgutils/sidecar/ -count=1`
Expected: PASS (live test skips without the env var).

- [ ] **Step 4: Commit**

```bash
git add internal/bgutils/sidecar/sidecar.go internal/bgutils/sidecar/sidecar_live_test.go
git commit -m "feat(sidecar): Go client GenerateGvsPoToken with provenance"
```

---

### Task 6: `PotProvider.GenerateGvsPoToken`

**Files:**
- Modify: `internal/bgutils/pot_provider.go` (counters :62-86, PotStats :101-135, `generateAndMint` :399-517)
- Test: `internal/bgutils/pot_provider_test.go`, `internal/bgutils/pot_provider_stats_test.go` (append)
- Modify: `docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md` (one sentence, see Step 4)

**Interfaces:**
- Consumes: `Sidecar.GenerateGvsPoToken` (Task 5).
- Produces:
  ```go
  type GvsMint struct {
      PoToken      string
      MinterSource string // "challenge" | "att_get" | "goja-fallback"
      MinterFresh  bool
      ViaSidecar   bool
  }
  func (pp *PotProvider) GenerateGvsPoToken(ctx context.Context, contentBinding, challenge string) (GvsMint, error)
  ```
  Plus `PotStats.GvsMints` / `PotStats.GvsMintsChallenge` (`json:"gvs_mints"` / `json:"gvs_mints_challenge"`).

- [ ] **Step 1: Write the failing tests**

Append to `pot_provider_test.go` (follow the file's existing constructor/logger conventions — it builds a `PotProvider` with `NewPotProvider` and a test logger):

```go
// TestGenerateGvsPoTokenNilSidecarFallsToGoja: with no sidecar attached the
// GVS path must fall through to the goja flow (which will fail fast in a
// unit test — no network — but MUST NOT touch the session cache).
func TestGenerateGvsPoTokenNilSidecar(t *testing.T) {
	pp := NewPotProvider(&BgConfig{}, testLogger(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Seed the session cache for this binding: a GVS mint must NOT return it.
	pp.mu.Lock()
	pp.sessionCache["bind1"] = &SessionData{PoToken: "cached-token", ContentBinding: "bind1", ExpiresAt: time.Now().Add(time.Hour)}
	pp.mu.Unlock()

	res, err := pp.GenerateGvsPoToken(ctx, "bind1", "")
	if err == nil {
		// Goja path unexpectedly succeeded (needs network) — even then the
		// result must not be the session-cached token.
		if res.PoToken == "cached-token" {
			t.Fatal("GVS mint returned session-cached token; cache must be bypassed")
		}
		if res.MinterSource != "goja-fallback" {
			t.Errorf("MinterSource = %q, want goja-fallback", res.MinterSource)
		}
		return
	}
	// Expected in unit-test conditions: goja mint fails (no network).
	// The session cache must be untouched and un-consulted.
	pp.mu.Lock()
	cached := pp.sessionCache["bind1"].PoToken
	pp.mu.Unlock()
	if cached != "cached-token" {
		t.Error("session cache mutated by GVS mint")
	}
}

func TestGvsMintCounters(t *testing.T) {
	pp := NewPotProvider(&BgConfig{}, testLogger(t))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _ = pp.GenerateGvsPoToken(ctx, "b", "")
	_, _ = pp.GenerateGvsPoToken(ctx, "b", `{"program":"p"}`)
	s := pp.Stats()
	if s.GvsMints != 2 {
		t.Errorf("GvsMints = %d, want 2", s.GvsMints)
	}
	if s.GvsMintsChallenge != 1 {
		t.Errorf("GvsMintsChallenge = %d, want 1", s.GvsMintsChallenge)
	}
}
```

(If the file has no `testLogger` helper, reuse whatever logger stub its existing tests construct — match the file, don't invent a new one.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/bgutils/ -run "TestGenerateGvsPoTokenNilSidecar|TestGvsMintCounters" -v`
Expected: FAIL (compile error, method undefined)

- [ ] **Step 3: Implement**

1. Factor the goja portion of `generateAndMint` (everything from `pp.minterCreatingMu.Lock()` at :431 to the end) into a new method with the identical body:

```go
// gojaGenerateAndMint is the goja fallback: serialize minter creation, then
// generate + cache + schedule eviction/refresh, then mint. Split out of
// generateAndMint so the GVS path can reuse it without re-attempting the
// sidecar. bypassCache=true skips the under-lock cache re-check AND forces a
// fresh minter (matching upstream TS bypass_cache semantics).
func (pp *PotProvider) gojaGenerateAndMint(ctx context.Context, contentBinding string, bypassCache bool) (*SessionData, error) {
	// ... body moved verbatim from generateAndMint :431-516 ...
}
```

`generateAndMint` keeps its sidecar block (:405-422) and ends with `return pp.gojaGenerateAndMint(ctx, contentBinding, bypassCache)`.

2. Add the GVS method + counters:

```go
// GvsMint is a GVS (segment-URL) PO-token mint result with the provenance
// the worker's "[POT] GVS mint" log line reports.
type GvsMint struct {
	PoToken      string
	MinterSource string // "challenge" | "att_get" | "goja-fallback"
	MinterFresh  bool
	ViaSidecar   bool
}

// GenerateGvsPoToken mints a segment-URL PO token under the
// fresh-minter-per-GVS-mint policy (spec §4.3): the session cache is fully
// bypassed (no read, no write), and the sidecar builds a fresh minter from
// the supplied watch-page challenge ("" → its /att/get fallback). The
// expensive BotGuard regeneration is deduplicated inside the sidecar
// (minterPromise), so no provider-side inflight entry is needed. When the
// sidecar is unavailable the goja flow runs with the challenge ignored —
// today's (session-incoherent) behavior, flagged as "goja-fallback".
func (pp *PotProvider) GenerateGvsPoToken(ctx context.Context, contentBinding, challenge string) (GvsMint, error) {
	pp.gvsMints.Add(1)
	if challenge != "" {
		pp.gvsMintsChallenge.Add(1)
	}
	pp.mu.Lock()
	sc := pp.sidecar
	pp.mu.Unlock()
	if sc != nil && sc.IsHealthy() {
		res, err := sc.GenerateGvsPoToken(ctx, contentBinding, challenge)
		if err == nil {
			pp.sidecarMintsHit.Add(1)
			return GvsMint{PoToken: res.PoToken, MinterSource: res.MinterSource, MinterFresh: res.MinterFresh, ViaSidecar: true}, nil
		}
		pp.sidecarMintsErr.Add(1)
		pp.logger.Warn("[PotProvider] sidecar GVS mint failed; falling through to goja", "err", err)
	}
	session, err := func() (s *SessionData, e error) {
		defer func() {
			if r := recover(); r != nil {
				s, e = nil, fmt.Errorf("bgutils GVS mint panic: %v", r)
				pp.logger.Error("[PotProvider] GVS mint panic", "panic", r)
			}
		}()
		return pp.gojaGenerateAndMint(ctx, contentBinding, true)
	}()
	if err != nil {
		pp.generateErrors.Add(1)
		return GvsMint{}, err
	}
	return GvsMint{PoToken: session.PoToken, MinterSource: "goja-fallback", MinterFresh: true}, nil
}
```

3. Counters + stats: add `gvsMints`, `gvsMintsChallenge atomic.Uint64` to the struct's counter block; add to `PotStats`:

```go
	GvsMints          uint64 `json:"gvs_mints"`
	GvsMintsChallenge uint64 `json:"gvs_mints_challenge"`
```

and populate both in `Stats()`.

- [ ] **Step 4: Amend one spec sentence**

In the spec §4.3, replace "Inflight dedup keyed on binding still applies so concurrent same-binding GVS calls share one mint." with: "Concurrent GVS mints are deduplicated inside the sidecar (`minterPromise` shares the expensive BotGuard regeneration); the provider adds no inflight entry of its own — per-binding token minting on a shared minter is cheap." (Implementation found the provider-side inflight entry would return a non-fresh result to the second caller, contradicting fresh-per-mint semantics.)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/bgutils/ -count=1`
Expected: PASS including the two new tests and existing stats tests.

- [ ] **Step 6: Commit**

```bash
git add internal/bgutils/pot_provider.go internal/bgutils/pot_provider_test.go internal/bgutils/pot_provider_stats_test.go docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md
git commit -m "feat(bgutils): GenerateGvsPoToken — fresh challenge-sourced GVS mints"
```

---### Task 7: Strategy wiring — videoID binding, challenge, provenance log, jobID on 403 lines

**Files:**
- Modify: `internal/worker/strategy_youtube_manifestless_dash.go` (:210-221 mint, :359-381 delete `manifestlessPotBinding`, :295-304 + :343-352 OnCipherFailure logs)
- Modify: `internal/worker/strategy_youtube_dash.go` (:97-109 mint, :279-289 + :310-320 OnCipherFailure logs)
- Modify: `internal/worker/strategy_youtube_hls.go` (:51-61 mint, :203-206 OnCipherFailure log)
- Modify: `internal/worker/strategies.go` (:237-252 `invalidate403Caches` jobID; :254-268 delete `poTokenBinding`; add `challengeLabel`)
- Test: `go test ./internal/worker/` (existing suite; fix any references to the deleted functions)

**Interfaces:**
- Consumes: `PotProvider.GenerateGvsPoToken` (Task 6), `VideoInfo.AttestationChallenge` (Task 3).
- Produces: the `[POT] GVS mint` provenance log line; `challengeLabel(challenge string) string` helper.

- [ ] **Step 1: Add the shared helper to `strategies.go`**

```go
// challengeLabel compresses a challenge value to the label the provenance
// log line reports: "page" (watch-page ytAtN challenge present) or "none".
func challengeLabel(challenge string) string {
	if challenge != "" {
		return "page"
	}
	return "none"
}
```

- [ ] **Step 2: Replace the manifestless mint block**

`strategy_youtube_manifestless_dash.go:210-221` becomes:

```go
	var pot string
	if potProvider != nil {
		mint, err := potProvider.GenerateGvsPoToken(ctx, job.Job.VideoID, videoInfo.AttestationChallenge)
		if err != nil {
			job.Logger.Warn("[POT] GVS mint failed", "jobID", job.Job.ID,
				"binding", "videoID", "challenge", challengeLabel(videoInfo.AttestationChallenge), "err", err)
		} else if mint.PoToken != "" {
			pot = mint.PoToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", "videoID", "challenge", challengeLabel(videoInfo.AttestationChallenge),
				"minterSource", mint.MinterSource, "minterFresh", mint.MinterFresh,
				"sidecar", mint.ViaSidecar, "tokenLength", len(mint.PoToken))
		}
	}
```

Delete `manifestlessPotBinding` (:359-381) entirely; its doc-comment rationale (videoID under the experiment) is superseded by the unconditional videoID binding — reference the spec in the mint block comment if desired.

- [ ] **Step 3: Replace the DASH-manifest and HLS mint calls**

`strategy_youtube_dash.go:97-109` — same shape, preserving the existing URL-append behavior:

```go
	var dashPoToken string
	if potProvider != nil {
		mint, err := potProvider.GenerateGvsPoToken(ctx, job.Job.VideoID, videoInfo.AttestationChallenge)
		if err != nil {
			job.Logger.Warn("[POT] GVS mint failed", "jobID", job.Job.ID,
				"binding", "videoID", "challenge", challengeLabel(videoInfo.AttestationChallenge), "err", err)
		} else if mint.PoToken != "" {
			dashPoToken = mint.PoToken
			dashURL = strings.TrimRight(dashURL, "/") + "/pot/" + mint.PoToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", "videoID", "challenge", challengeLabel(videoInfo.AttestationChallenge),
				"minterSource", mint.MinterSource, "minterFresh", mint.MinterFresh,
				"sidecar", mint.ViaSidecar, "tokenLength", len(mint.PoToken))
		} else {
			job.Logger.Warn("[POT] generator returned empty token", "jobID", job.Job.ID)
		}
	}
```

`strategy_youtube_hls.go:51-61` — identical pattern with `hlsPoToken` / `hlsURL`.

Then delete `poTokenBinding` from `strategies.go` (:254-268) and run `grep -rn "poTokenBinding\|manifestlessPotBinding" internal/` — fix every remaining reference (there is at least the orchestrator strategy test file if it stubs bindings; adjust or delete those assertions).

- [ ] **Step 4: Add jobID to the 403-signal lines**

- `strategies.go:238`: `job.Logger.Warn("[Cipher] "+tag+" 403 signal — invalidating solver and POT", "jobID", job.Job.ID, "playerURL", playerURL)`
- Every OnCipherFailure closure's Warn/Info lines gain `"jobID", job.Job.ID` as the FIRST key-value pair: manifestless video (:299, :302), manifestless audio (:347, :350), dash video (~:281-285), dash audio (~:312-316), hls (~:204-206). Match each line's existing message text; only append the arg pair.

- [ ] **Step 5: Build, fix fallout, test**

Run: `go build ./... && go test ./internal/worker/ -count=1`
Expected: PASS after fixing any test referencing the deleted binding helpers.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/
git commit -m "feat(worker): GVS mints via job challenge + videoID binding, provenance + jobID logs"
```

---

### Task 8: Job-scoped engine downloader logging

**Files:**
- Create: `internal/worker/scoped_logger.go`
- Test: `internal/worker/scoped_logger_test.go`
- Modify: every worker `NewSegmentDownloader(engine.DownloaderOptions{...})` construction: `strategy_youtube_manifestless_dash.go:267,321`, `strategy_youtube_dash.go:258,289`, `strategy_youtube_hls.go:178`, `strategy_youtube_vod.go:196,207`, `orchestrator_twitch.go:221`

**Interfaces:**
- Produces: `newScopedLogger(inner logger, args ...any) *scopedLogger`, satisfying both the worker's anonymous `logger` interface and `engine.DownloaderLogger`.

- [ ] **Step 1: Write the failing test**

`internal/worker/scoped_logger_test.go`:

```go
package worker

import (
	"reflect"
	"testing"
)

type captureLogger struct {
	msgs [][]any
}

func (c *captureLogger) log(msg string, args []any) { c.msgs = append(c.msgs, append([]any{msg}, args...)) }
func (c *captureLogger) Debug(msg string, args ...any) { c.log(msg, args) }
func (c *captureLogger) Info(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Warn(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Error(msg string, args ...any) { c.log(msg, args) }

func TestScopedLoggerAppendsFixedArgs(t *testing.T) {
	cap := &captureLogger{}
	sl := newScopedLogger(cap, "jobID", "abc123", "stream", "video")
	sl.Info("segment fetched", "seq", 42)
	want := []any{"segment fetched", "seq", 42, "jobID", "abc123", "stream", "video"}
	if len(cap.msgs) != 1 || !reflect.DeepEqual(cap.msgs[0], want) {
		t.Errorf("got %v, want %v", cap.msgs, want)
	}
	// No-arg call still carries the scope.
	sl.Warn("stopped")
	want = []any{"stopped", "jobID", "abc123", "stream", "video"}
	if !reflect.DeepEqual(cap.msgs[1], want) {
		t.Errorf("got %v, want %v", cap.msgs[1], want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/worker/ -run TestScopedLogger -v`
Expected: FAIL (compile error)

- [ ] **Step 3: Implement**

`internal/worker/scoped_logger.go`:

```go
package worker

// scopedLogger wraps a job logger, appending fixed key-value pairs to every
// call, so engine components — which know nothing about jobs — emit
// attributable lines. Motivated by the 2026-08-14 premiere investigation,
// where the engine's per-segment 403 lines carried no jobID and interleaved
// video/audio downloaders were indistinguishable. Satisfies both the worker's
// anonymous logger interface and engine.DownloaderLogger (same four methods).
type scopedLogger struct {
	inner logger
	args  []any
}

func newScopedLogger(inner logger, args ...any) *scopedLogger {
	return &scopedLogger{inner: inner, args: args}
}

// The variadic slice is freshly allocated per call, so appending the fixed
// args to it cannot alias a caller's backing array.
func (s *scopedLogger) Debug(msg string, args ...any) { s.inner.Debug(msg, append(args, s.args...)...) }
func (s *scopedLogger) Info(msg string, args ...any)  { s.inner.Info(msg, append(args, s.args...)...) }
func (s *scopedLogger) Warn(msg string, args ...any)  { s.inner.Warn(msg, append(args, s.args...)...) }
func (s *scopedLogger) Error(msg string, args ...any) { s.inner.Error(msg, append(args, s.args...)...) }
```

(If the worker package's anonymous logger interface type is not named `logger`, match whatever `JobContext.Logger`'s declared type is — see `worker.go:80`.)

- [ ] **Step 4: Wire into every downloader construction**

In each listed `DownloaderOptions{...}` literal, replace `Logger: job.Logger,` with:

- manifestless video (:267): `Logger: newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),`
- manifestless audio (:321): `... "stream", "audio"),`
- dash video/audio (:258/:289): same video/audio pair
- hls (:178): `"stream", "video"` (HLS variant carries muxed A/V; label stays "video")
- vod video/audio (:196/:207): same video/audio pair
- twitch (:221): `Logger: newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),` (single muxed downloader)

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/worker/ -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/worker/scoped_logger.go internal/worker/scoped_logger_test.go internal/worker/strategy_youtube_manifestless_dash.go internal/worker/strategy_youtube_dash.go internal/worker/strategy_youtube_hls.go internal/worker/strategy_youtube_vod.go internal/worker/orchestrator_twitch.go
git commit -m "feat(worker): job+stream scoped loggers for engine downloaders"
```

---

### Task 9: Docs, spec resolution, full verification

**Files:**
- Modify: `docs/spec/platform-services.md` (BotGuard / PO token section)
- Modify: `docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md` (§7)

- [ ] **Step 1: Resolve spec §7**

Replace §7's open question with its answer (found during Task 7): `invalidate403Caches` (`internal/worker/strategies.go`) only wipes caches; `DownloaderOptions.PoToken` is static after construction and `OnCipherFailure` returns only a fresh URL — there is **no** mid-job POT re-mint today. Per the spec's decision rule, no rotation was added; it remains part of the deferred blanket-403 failover work.

- [ ] **Step 2: Update `docs/spec/platform-services.md`**

In the BotGuard/PO-token section, document (prose matching the file's existing style):
- GVS (segment-URL) tokens: minted per download start via `PotProvider.GenerateGvsPoToken` — fresh minter per mint, built from the watch page's `window.ytAtN` attestation challenge when present (sidecar `/att/get` fallback otherwise), content binding = video ID.
- Player-API tokens: unchanged (cached minter, visitorData binding).
- The `[POT] GVS mint` provenance log line and what each field means.
- The sidecar RPC additions (`challenge`, `freshMinter`, `minterSource`, `minterFresh`).

- [ ] **Step 3: Full verification gates**

```bash
cd bgutil-sidecar && node build.mjs && cd ..
go build ./... && go vet ./... && go test ./... -count=1
```

Expected: all pass. Optionally run the live gates if network testing is wanted now:
`MOOMBOX_LIVE_BG_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarLiveGvsMint" ./internal/bgutils/sidecar/`

- [ ] **Step 4: Commit**

```bash
git add docs/spec/platform-services.md docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md
git commit -m "docs: attestation POT coherence — platform-services update, spec §7 resolved"
```

- [ ] **Step 5: Delete the planning artifacts (owner policy)**

Per the owner's delete-implemented-plans policy (2026-08-14): once every task above is committed and verification passes, delete this plan AND the debugging design spec — git history preserves both. Field-verification gates (next live stream, next premiere) are tracked in project memory, not in the repo.

```bash
git rm docs/superpowers/plans/2026-08-14-attestation-pot-coherence.md docs/superpowers/specs/2026-08-14-attestation-pot-coherence-design.md
git commit -m "chore: remove implemented attestation POT coherence plan + spec"
```

---

## Post-implementation (not part of this plan)

- **Regression gate (days):** first regular live capture must download normally; the `[POT] GVS mint` line should show `binding=videoID`, `minterSource=challenge` (or `att_get` if the page had no ytAtN), `sidecar=true`.
- **Fix trial (possibly months):** next premiere. If it still 403s, the provenance line identifies the configuration; datasync-ID binding is the next suspect (spec §3).
- Release/version bump: owner-controlled; not included here.

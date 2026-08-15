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
		// Regression: Python's (?<!\.) blocks the outer octal alternative
		// here, but the decimal-key alternative's lookahead still finds the
		// trailing ':' and matches just the digits; INTEGER_TABLE re-tests
		// those bare digits (no dot guard there) and still resolves them as
		// octal (63), while the ':' itself passes through unmatched/raw —
		// so the value stays base-8 but is never folded into a quoted key.
		// Python: js_to_json('{1.077:5}') == '{1.63:5}'.
		{"octal_dot_lookbehind_fallthrough", `{1.077:5}`, `{1.63:5}`},
		{"template_iife", "{a: `${e(\"\")}`}", `{"a": "\"e\"(\"\")"}`},
		{"template_var", "`Hello ${name}`", `"Hello world"`},        // vars: name -> "world" (set below)
		{"template_var_twice", "`${name}${name}`", `"XX"`},          // vars: name -> "X"
		{"template_num_twice", "`${name}${name}`", `"55"`},          // vars: name -> 5
		{"template_num_quoted", "`${name}\"${name}\"`", `"5\"5\""`}, // vars: name -> 5
		{"template_unresolved", "`${name}`", `"name"`},              // no vars
		// Regression: Python's int() is arbitrary-precision, so hex/octal
		// literals beyond int64/uint64 range still convert to their exact
		// decimal digits instead of aborting the whole conversion with a
		// range error (see js_to_json fix_kv: i = int(im.group(1), base)).
		// 0xFFFFFFFFFFFFFFFF (16 Fs) == 2**64-1 as a KEY -> quoted decimal.
		{"hex_key_huge", `{0xFFFFFFFFFFFFFFFF:1}`, `{"18446744073709551615":1}`},
		// Same magnitude (> 2**63, the old int64 ceiling) as a VALUE ->
		// bare decimal, unquoted.
		{"hex_value_large", `{a:0xFFFFFFFFFFFFFFFF}`, `{"a":18446744073709551615}`},
		// Octal literal exceeding even uint64's range (> 2**64-1),
		// forcing the math/big fallback rather than the uint64 fast path.
		{"octal_key_beyond_uint64", `{07777777777777777777777:1}`, `{"73786976294838206463":1}`},
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
		// Regression: Python's json.dumps coerces a None dict key to the
		// string "null" (json/encoder.py _iterencode_dict), not Go's
		// fmt-default "<nil>". Python: js_to_json('new Map([[null, 5]])')
		// == '{"null": 5}'.
		{"map_null_key", `new Map([[null, 5]])`, `{"null": 5}`},
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

package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSkipJSONValueSteppingOverNestedShapes: the walkers exist so a reader can
// reach one key of a huge object without decoding the rest.
//
// Mutant: dropping the depth++ arm from FinishJSONValue leaves the decoder
// inside the nested object and the "wanted" token comes back as something else.
func TestSkipJSONValueSteppingOverNestedShapes(t *testing.T) {
	const doc = `{"a":{"deep":[1,{"x":"}"}]},"b":[[],[{}]],"c":"scalar","wanted":42}`
	dec := json.NewDecoder(strings.NewReader(doc))
	if _, err := dec.Token(); err != nil { // the opening '{'
		t.Fatal(err)
	}
	for {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			t.Fatalf("ran out of keys before reaching \"wanted\": %v", keyTok)
		}
		if key == "wanted" {
			break
		}
		if err := SkipJSONValue(dec); err != nil {
			t.Fatalf("SkipJSONValue(%q): %v", key, err)
		}
	}
	var got int
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode the value after the skips: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42 — the skips left the decoder in the wrong place", got)
	}
}

// TestFinishJSONValueReportsATruncatedDocument: a truncated composite must
// surface as an error, not as a silent success.
//
// Mutant: returning nil on dec.Token()'s error makes this pass a broken file.
func TestFinishJSONValueReportsATruncatedDocument(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"a":[1,2`))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	if err := FinishJSONValue(dec, 1); err == nil {
		t.Error("a truncated document must error")
	}
}

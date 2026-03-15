package cipher

import (
	"net/url"
	"testing"
)

func TestRawQueryParam(t *testing.T) {
	tests := []struct {
		name        string
		rawQuery    string
		key         string
		wantRaw     string
		wantDecoded string
	}{
		{
			name:        "simple value",
			rawQuery:    "n=abc123&foo=bar",
			key:         "n",
			wantRaw:     "abc123",
			wantDecoded: "abc123",
		},
		{
			name:        "percent-encoded slash",
			rawQuery:    "n=abc%2Fdef&foo=bar",
			key:         "n",
			wantRaw:     "abc%2Fdef",
			wantDecoded: "abc/def",
		},
		{
			name:        "percent-encoded equals",
			rawQuery:    "foo=bar&n=abc%3Ddef",
			key:         "n",
			wantRaw:     "abc%3Ddef",
			wantDecoded: "abc=def",
		},
		{
			name:        "percent-encoded percent",
			rawQuery:    "n=abc%25def&other=1",
			key:         "n",
			wantRaw:     "abc%25def",
			wantDecoded: "abc%def",
		},
		{
			name:        "multiple special chars",
			rawQuery:    "n=eabGF%2Fps%25UK%3DuWHXGh6FR4&x=1",
			key:         "n",
			wantRaw:     "eabGF%2Fps%25UK%3DuWHXGh6FR4",
			wantDecoded: "eabGF/ps%UK=uWHXGh6FR4",
		},
		{
			name:        "first param",
			rawQuery:    "n=first&a=second",
			key:         "n",
			wantRaw:     "first",
			wantDecoded: "first",
		},
		{
			name:        "missing key",
			rawQuery:    "foo=bar&baz=qux",
			key:         "n",
			wantRaw:     "",
			wantDecoded: "",
		},
		{
			name:        "empty query",
			rawQuery:    "",
			key:         "n",
			wantRaw:     "",
			wantDecoded: "",
		},
		{
			name:        "similar key name not matched",
			rawQuery:    "nn=wrong&sn=also_wrong&n=right",
			key:         "n",
			wantRaw:     "right",
			wantDecoded: "right",
		},
		{
			name:        "empty value",
			rawQuery:    "n=&foo=bar",
			key:         "n",
			wantRaw:     "",
			wantDecoded: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRaw, gotDecoded := RawQueryParam(tt.rawQuery, tt.key)
			if gotRaw != tt.wantRaw {
				t.Errorf("raw: got %q, want %q", gotRaw, tt.wantRaw)
			}
			if gotDecoded != tt.wantDecoded {
				t.Errorf("decoded: got %q, want %q", gotDecoded, tt.wantDecoded)
			}
		})
	}
}

func TestNParamReplacementWithEncodedChars(t *testing.T) {
	// Simulate the n-param replacement logic used in ResolveURL and decryptNParam.
	// Verify that percent-encoded n-params in URLs are correctly matched and replaced.
	tests := []struct {
		name    string
		rawURL  string
		wantOld string // the raw n= substring that should be found
	}{
		{
			name:    "plain n-param",
			rawURL:  "https://example.com/v?n=abc123&sig=xyz",
			wantOld: "&n=abc123",
		},
		{
			name:    "encoded slash in n-param",
			rawURL:  "https://example.com/v?foo=1&n=abc%2Fdef&sig=xyz",
			wantOld: "&n=abc%2Fdef",
		},
		{
			name:    "encoded percent in n-param",
			rawURL:  "https://example.com/v?n=abc%25def",
			wantOld: "?n=abc%25def",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}

			rawN, decodedN := RawQueryParam(u.RawQuery, "n")
			if rawN == "" {
				t.Fatal("expected non-empty rawN")
			}
			if decodedN == "" {
				t.Fatal("expected non-empty decodedN")
			}

			// Verify the raw form matches in the URL
			found := false
			for _, prefix := range []string{"?", "&"} {
				old := prefix + "n=" + rawN
				if old == tt.wantOld {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("raw n-param %q did not produce expected match %q", rawN, tt.wantOld)
			}

			// Verify that Query().Get() would NOT match the raw URL for encoded values
			decodedFromQuery := u.Query().Get("n")
			if decodedFromQuery != decodedN {
				t.Errorf("Query().Get() returned %q but RawQueryParam decoded to %q", decodedFromQuery, decodedN)
			}
		})
	}
}

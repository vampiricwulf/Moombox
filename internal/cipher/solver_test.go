package cipher

import (
	"testing"
)

func TestGetFromPrepared(t *testing.T) {
	// Test with a simple sig function that reverses a string
	code := `
_result.sig = function(input) {
    return input.split("").reverse().join("");
};
_result.n = function(input) {
    return input + "_transformed";
};
`

	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	if solvers.Sig == nil {
		t.Fatal("expected sig solver")
	}
	if solvers.N == nil {
		t.Fatal("expected n solver")
	}

	// Test sig
	sigResult, err := solvers.Sig("hello")
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if sigResult != "olleh" {
		t.Errorf("sig: expected 'olleh', got %q", sigResult)
	}

	// Test n
	nResult, err := solvers.N("test")
	if err != nil {
		t.Fatalf("n: %v", err)
	}
	if nResult != "test_transformed" {
		t.Errorf("n: expected 'test_transformed', got %q", nResult)
	}
}

func TestGetFromPrepared_SigOnly(t *testing.T) {
	code := `
_result.sig = function(input) {
    return input.toUpperCase();
};
`

	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	if solvers.Sig == nil {
		t.Fatal("expected sig solver")
	}
	if solvers.N != nil {
		t.Error("expected nil n solver")
	}

	result, err := solvers.Sig("hello")
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if result != "HELLO" {
		t.Errorf("expected HELLO, got %q", result)
	}
}

func TestStsRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`signatureTimestamp:19850`, "19850"},
		{`sts:20123`, "20123"},
		{`signatureTimestamp: 20456`, "20456"},
	}

	for _, tt := range tests {
		m := stsRegex.FindStringSubmatch(tt.input)
		if m == nil {
			t.Errorf("no match for %q", tt.input)
			continue
		}
		if m[1] != tt.expected {
			t.Errorf("expected %q, got %q for input %q", tt.expected, m[1], tt.input)
		}
	}
}

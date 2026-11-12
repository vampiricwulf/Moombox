package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A cut of nodejs.org/dist/index.json: newest first, other major lines mixed
// in, as the real file has them.
const sampleIndex = `[
	{"version":"v25.3.0","date":"2026-11-01","security":true},
	{"version":"v24.23.1","date":"2026-11-05","security":true,"lts":"Krypton"},
	{"version":"v24.23.0","date":"2026-11-02","security":false,"lts":"Krypton"},
	{"version":"v24.22.3","date":"2026-10-31","security":true,"lts":"Krypton"},
	{"version":"v24.22.2","date":"2026-10-28","security":false,"lts":"Krypton"},
	{"version":"v24.22.1","date":"2026-10-20","security":true,"lts":"Krypton"},
	{"version":"v22.30.0","date":"2026-10-21","security":true,"lts":"Jod"}
]`

func TestNewerSecurityReleases(t *testing.T) {
	cases := []struct {
		name string
		pin  string
		want []string
	}{
		{"two newer security releases, oldest first", "v24.22.2", []string{"v24.22.3", "v24.23.1"}},
		{"a newer release that is not a security release is not a finding", "v24.23.0", []string{"v24.23.1"}},
		{"the pin is the newest security release", "v24.23.1", nil},
		{"an older security release and other major lines are ignored", "v22.30.0", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newerSecurityReleases([]byte(sampleIndex), tc.pin)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Numbers compare as numbers: v24.9.0 is older than v24.22.2.
func TestNewerSecurityReleasesComparesNumerically(t *testing.T) {
	index := `[{"version":"v24.22.2","security":false},{"version":"v24.9.0","security":true}]`
	got, err := newerSecurityReleases([]byte(index), "v24.22.2")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no finding", got, err)
	}
}

func TestNewerSecurityReleasesRefusesAnIndexItCannotJudge(t *testing.T) {
	cases := map[string]string{
		"not JSON":             `<html>503</html>`,
		"an object, not a list": `{"version":"v24.22.2"}`,
		"pin not listed":       `[{"version":"v24.23.1","security":true}]`,
	}
	for name, index := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := newerSecurityReleases([]byte(index), "v24.22.2"); err == nil {
				t.Fatalf("got %v with no error; an unreadable index must not pass as clean", got)
			}
		})
	}
}

func TestCheckSecurity(t *testing.T) {
	index := func() ([]byte, error) { return []byte(sampleIndex), nil }

	t.Run("a newer security release fails and names it", func(t *testing.T) {
		var out bytes.Buffer
		if code := checkSecurity("v24.22.2", index, &out); code != securityCheckFailed {
			t.Fatalf("exit code %d, want %d", code, securityCheckFailed)
		}
		if s := out.String(); !strings.HasPrefix(s, "::error::") || !strings.Contains(s, "v24.22.3, v24.23.1") {
			t.Fatalf("output does not name the releases as an error annotation: %q", s)
		}
	})

	t.Run("an up-to-date pin passes", func(t *testing.T) {
		var out bytes.Buffer
		if code := checkSecurity("v24.23.1", index, &out); code != securityCheckOK {
			t.Fatalf("exit code %d, want %d: %s", code, securityCheckOK, out.String())
		}
	})

	t.Run("a failed fetch warns and passes", func(t *testing.T) {
		var out bytes.Buffer
		down := func() ([]byte, error) { return nil, errors.New("dial tcp: i/o timeout") }
		if code := checkSecurity("v24.22.2", down, &out); code != 0 {
			t.Fatalf("exit code %d, want 0: nodejs.org being down is not a finding", code)
		}
		if s := out.String(); !strings.HasPrefix(s, "::warning::") {
			t.Fatalf("a skipped check must say so as a warning annotation: %q", s)
		}
	})

	t.Run("an index that cannot be judged fails", func(t *testing.T) {
		var out bytes.Buffer
		garbage := func() ([]byte, error) { return []byte("<html>"), nil }
		if code := checkSecurity("v24.22.2", garbage, &out); code != securityCheckFailed {
			t.Fatalf("exit code %d, want %d", code, securityCheckFailed)
		}
	})
}

// The pin the check reads must be one it can parse, or every weekly run
// fails on the pin instead of on a finding.
func TestPinnedNodeVersionParses(t *testing.T) {
	if _, err := parseNodeVersion(nodeVersion); err != nil {
		t.Fatal(err)
	}
}

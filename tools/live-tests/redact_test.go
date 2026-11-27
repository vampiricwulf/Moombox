package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The leak review's holes 1-5, each with made-up values.
func TestRedactClosesTheReviewedHoles(t *testing.T) {
	const (
		spaced  = "SpacedVALUEwithTRAILINGblank0123"
		slashed = "Sl/ash=Plus+Value&Amp0123456789"
		renewed = "BrandNewRenewedValueNotInAnyFile99"
	)
	dir := t.TempDir()
	p := filepath.Join(dir, "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" +
		"  .youtube.com\tTRUE\t/\tTRUE\t1999999999\tSID\t" + spaced + " \t \r\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1999999999\tAPISID\t" + slashed + "\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1999999999\tSAPISID\t" + fakeSID + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"trailing blank":    "value " + spaced + " \t end",
		"quoted cookie":     `"cookie" = "OTHER=` + "x0x0x0x0x0x0x0x0x0" + `"`,
		"quoted set-cookie": `"set-cookie" = "__Secure-1PSIDTS=` + "y1y1y1y1y1y1y1y1y1" + `; Path=/"`,
		"escaped json":      `{\"cookie\":\"Z=` + "z2z2z2z2z2z2z2z2" + `\"}`,
		"renewed by name":   "kept __Secure-3PSIDCC=" + renewed + "; Path=/",
		"base64":            "b64 " + base64.StdEncoding.EncodeToString([]byte(fakeSID)),
		"base64 url raw":    "b64 " + base64.RawURLEncoding.EncodeToString([]byte(slashed)),
		"hex":               "hex " + hex.EncodeToString([]byte(fakeSID)),
		"js escapes":        "js " + strings.NewReplacer("/", `\/`, "=", `\u003d`, "+", `\u002b`, "&", `\u0026`).Replace(slashed),
		"cut prefix":        "error: ..." + fakeSID[:20],
		"cut suffix":        fakeSID[len(fakeSID)-18:] + " was cut",
	}
	secrets := []string{spaced, slashed, renewed, fakeSID, "x0x0x0x0x0x0x0x0x0", "y1y1y1y1y1y1y1y1y1", "z2z2z2z2z2z2z2z2",
		base64.StdEncoding.EncodeToString([]byte(fakeSID)), hex.EncodeToString([]byte(fakeSID)), fakeSID[:20], fakeSID[len(fakeSID)-18:]}
	for name, line := range cases {
		var out bytes.Buffer
		if err := runRedact([]string{"--cookies", p}, strings.NewReader(line+"\n"), &out); err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if strings.Contains(line, s) && strings.Contains(out.String(), s) {
				t.Errorf("%s: a secret survived: %q", name, out.String())
			}
		}
		if !strings.Contains(out.String(), mask) {
			t.Errorf("%s: nothing masked: %q", name, out.String())
		}
	}

	// A value split across two lines, both parts under fragLen, the second
	// behind go test's indent.
	in := "    x_test.go:1: tail " + slashed[:12] + "\n        " + slashed[12:] + " rest\n"
	var out bytes.Buffer
	if err := runRedact([]string{"--cookies", p}, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), slashed[:12]) || strings.Contains(out.String(), slashed[12:]) {
		t.Errorf("a split value survived: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), " rest\n") {
		t.Errorf("the words after a split value were lost: %q", out.String())
	}

	// The scan's own reading finds what an unredacted log holds, the split
	// and the renewed value included, and nothing in the redacted one.
	raw := strings.Join(append(mapValues(cases), in), "\n")
	logRaw := filepath.Join(dir, "raw.log")
	os.WriteFile(logRaw, []byte(raw), 0o600)
	var sc bytes.Buffer
	n, err := runScan([]string{"-v", "--cookies", p, logRaw}, &sc)
	if err != nil {
		t.Fatal(err)
	}
	if n < len(cases) || !strings.Contains(sc.String(), "joined") || !strings.Contains(sc.String(), "named") {
		t.Errorf("scan of the unredacted log found %d (want %d+, a joined and a named hit):\n%s", n, len(cases), sc.String())
	}
	var red bytes.Buffer
	runRedact([]string{"--cookies", p}, strings.NewReader(raw), &red)
	logRed := filepath.Join(dir, "red.log")
	os.WriteFile(logRed, red.Bytes(), 0o600)
	sc.Reset()
	if n, err := runScan([]string{"-v", "--cookies", p, logRed}, &sc); err != nil || n != 0 {
		t.Errorf("scan of the redacted log = %d, %v; want 0:\n%s", n, err, sc.String())
	}
	if strings.Contains(sc.String(), "x0x0") {
		t.Error("the scan printed a secret")
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// The runner's own lines and test verdicts pass through untouched.
func TestRedactLeavesOrdinaryOutputAlone(t *testing.T) {
	p := fakeCookieFile(t, t.TempDir())
	in := "### live-tests begin group=youtube pkg=internal/youtube\n" +
		"=== RUN   TestLivePremiumVerdict\n" +
		"    premium_live_test.go:60: LIVE-SESSION youtube=signed_in premium=yes\n" +
		"    x_test.go:9: load cookie file: open C:\\x\\work-1.txt\n" +
		"--- PASS: TestLivePremiumVerdict (1.20s)\n" +
		"ok  \tgithub.com/x/internal/youtube\t1.5s\n"
	var out bytes.Buffer
	if err := runRedact([]string{"--cookies", p}, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Errorf("ordinary output changed:\n%s", out.String())
	}
}

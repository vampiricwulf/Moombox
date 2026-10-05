package cookies

import (
	"os"
	"strings"
	"testing"
)

// TestABrowserReadsEmptyRowDoesNotEvictAWorkingOne: the readers report an
// empty-valued cookie as a row (a Firefox NULL value, a Chromium row whose
// encrypted column is empty), and the browser refresh and setup merged them
// over cookies.txt, where a row wins by name+domain+path — so an empty
// LOGIN_INFO replaced a working one and the jar was signed out. Both merges
// go through mergeBrowserCookies, which leaves such rows out.
//
// Mutants: mergeBrowserCookies without stripEmptyValuedRows (the jar loses
// LOGIN_INFO); either call site back on bare mergeCookieFiles (the pin fails).
func TestABrowserReadsEmptyRowDoesNotEvictAWorkingOne(t *testing.T) {
	existing := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-good\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-good\n"
	rows := deduplicateAndFormat([]extractedCookie{
		{domain: ".youtube.com", path: "/", secure: true, name: "SAPISID", value: "sapisid-fresh"},
		{domain: ".youtube.com", path: "/", secure: true, name: "LOGIN_INFO", value: ""},
	})
	fetched := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"

	j := NewCookieJar()
	j.loadFrom([]byte(mergeBrowserCookies(existing, fetched)), "")
	if got := j.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "login-good" {
		t.Errorf("LOGIN_INFO = %q, want the working login-good", got)
	}
	if got := j.GetCookieFor(PlatformYouTube, "SAPISID"); got != "sapisid-fresh" {
		t.Errorf("SAPISID = %q, want the fresh value the browser read", got)
	}

	for _, file := range []string{"autocookies_refresh.go", "autocookies_setup.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "mergeBrowserCookies(") || strings.Contains(string(src), "mergeCookieFiles(") {
			t.Errorf("%s does not merge the browser's cookies through mergeBrowserCookies", file)
		}
	}
}

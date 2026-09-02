package cookies

import (
	"errors"
	"strings"
	"testing"
)

// Fixtures. Every value here is obviously fake and every expiry is far in the
// future — mergeCookieFiles PRUNES a row whose expiry has passed (rowExpired,
// autocookies_merge.go:217), so a fixture written with a past timestamp would
// vanish mid-merge and the test would be asserting the pruner.
const (
	fakeYouTubeRows = ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-sapisid-aaaa\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-aaaa\n"
	fakeTwitchRows = ".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\tfake-authtoken-aaaa\n" +
		".twitch.tv\tTRUE\t/\tFALSE\t2000000000\tlogin\tfake-login-aaaa\n"
	// What a signed-OUT YouTube export looks like: entirely well-formed, and
	// not one row is a credential. This is the shape the third refusal exists
	// for, and it is the one users actually paste.
	fakeSignedOutRows = ".youtube.com\tTRUE\t/\tFALSE\t2000000000\tYSC\tfake-ysc-aaaa\n" +
		".youtube.com\tTRUE\t/\tFALSE\t2000000000\tVISITOR_INFO1_LIVE\tfake-visitor-aaaa\n"
	netscapeHeader = "# Netscape HTTP Cookie File\n"
)

// TestPrepareCookieImportRefusesTheThreeShapes.
//
// The mutations, one per row: deleting the unparseable-count arm (a JSON paste
// then reports "no cookie rows", which sends the operator looking for cookies
// in a file that is not a cookie file at all); deleting the no-rows arm (a
// header-only export falls through to the credential probe and reports the
// wrong cause); deleting the netscapeCookiesHoldACredential call (a signed-out
// export is WRITTEN, its YSC row wins the merge, and the operator is told the
// import succeeded — the exact failure the endpoint exists to report at paste
// time).
func TestPrepareCookieImportRefusesTheThreeShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		incoming string
		want     error
	}{
		{
			name:     "a JSON export from a cookie extension",
			incoming: `[{"domain":".youtube.com","name":"SAPISID","value":"fake-sapisid-aaaa"}]`,
			want:     ErrImportNotNetscape,
		},
		{
			name:     "an HTML error page",
			incoming: "<!doctype html>\n<html><body>404 Not Found</body></html>\n",
			want:     ErrImportNotNetscape,
		},
		{
			name:     "the header and nothing else",
			incoming: netscapeHeader + "# https://curl.se/docs/http-cookies.html\n\n",
			want:     ErrImportNoRows,
		},
		{
			name:     "an export from a signed-out window",
			incoming: netscapeHeader + fakeSignedOutRows,
			want:     ErrImportNoCredential,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prepareCookieImport("", tc.incoming)
			if !errors.Is(err, tc.want) {
				t.Fatalf("prepareCookieImport error = %v, want %v", err, tc.want)
			}
			if got != "" {
				t.Errorf("a refused import still produced %d bytes to write — nothing may be "+
					"written for a paste that was rejected", len(got))
			}
		})
	}
}

// TestPrepareCookieImportRefusalsNameNoValue is the security rule at the one
// place a rejection message is composed. Every sentinel's text is a fixed
// string today, so the property is structural — but an arm that grew a "the
// row that failed was %s" would put a credential in an HTTP response body.
//
// The mutation: any sentinel reworded to interpolate the offending row.
func TestPrepareCookieImportRefusalsNameNoValue(t *testing.T) {
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa", "fake-ysc-aaaa"}
	for _, incoming := range []string{
		`[{"name":"SAPISID","value":"fake-sapisid-aaaa"}]`,
		netscapeHeader + fakeSignedOutRows,
		netscapeHeader,
	} {
		_, err := prepareCookieImport(fakeYouTubeRows+fakeTwitchRows, incoming)
		if err == nil {
			t.Fatalf("expected a refusal for a %d-byte paste", len(incoming))
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the refusal message carries a cookie value: %q", err.Error())
			}
		}
	}
}

// TestPrepareCookieImportMergesRatherThanReplaces is the S9 ruling, both ways
// round. A YouTube-only paste must leave every Twitch row exactly as it was,
// and a Twitch-only paste must leave YouTube alone.
//
// Asserted against a real jar loaded from the produced text — a test that only
// checked the returned string for a substring would pass against a merge that
// produced an unloadable file.
//
// The mutation: `return cleaned, nil` instead of merging — the sibling
// platform's rows disappear from the file, silently, and its next capture fails
// for an unrelated-looking reason.
func TestPrepareCookieImportMergesRatherThanReplaces(t *testing.T) {
	existing := netscapeHeader + fakeYouTubeRows + fakeTwitchRows

	t.Run("a youtube-only paste keeps twitch", func(t *testing.T) {
		fresh := ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-sapisid-bbbb\n" +
			".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-bbbb\n"
		out, err := prepareCookieImport(existing, netscapeHeader+fresh)
		if err != nil {
			t.Fatalf("prepareCookieImport: %v", err)
		}
		jar := NewCookieJar()
		jar.loadFrom([]byte(out), "")
		if !jar.HasAnyTwitchAuthCookie() {
			t.Error("the merged file holds no Twitch auth cookie — a YouTube paste destroyed the " +
				"Twitch session, which is the exact finding this endpoint was required to avoid")
		}
		if got := jar.GetCookieFor(PlatformTwitch, "auth-token"); got != "fake-authtoken-aaaa" {
			t.Errorf("twitch auth-token = %q, want the untouched existing row", got)
		}
		if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "fake-sapisid-bbbb" {
			t.Errorf("youtube SAPISID = %q, want the pasted value — the new row must win by name+domain", got)
		}
	})

	t.Run("a twitch-only paste keeps youtube", func(t *testing.T) {
		fresh := ".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\tfake-authtoken-bbbb\n"
		out, err := prepareCookieImport(existing, netscapeHeader+fresh)
		if err != nil {
			t.Fatalf("prepareCookieImport: %v", err)
		}
		jar := NewCookieJar()
		jar.loadFrom([]byte(out), "")
		if got := jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "fake-logininfo-aaaa" {
			t.Errorf("youtube LOGIN_INFO = %q, want the untouched existing row", got)
		}
		if got := jar.GetCookieFor(PlatformTwitch, "auth-token"); got != "fake-authtoken-bbbb" {
			t.Errorf("twitch auth-token = %q, want the pasted value", got)
		}
	})
}

// TestPrepareCookieImportKeysByNameAndDomain. A .google.com row and a
// .youtube.com row of the SAME NAME are two different cookies, and a merge
// keyed by name alone destroys one of them before the file is ever written —
// somewhere CookieJar.Load's domain-aware admission can never reach, because a
// row that was never written is a row the jar never sees.
//
// The mutation: keying the merge by bare name (or "simplifying"
// mergeCookieFiles' cookieKey to drop its domain field).
func TestPrepareCookieImportKeysByNameAndDomain(t *testing.T) {
	existing := netscapeHeader +
		".google.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-google-sapisid-aaaa\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-yt-sapisid-aaaa\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-aaaa\n"
	fresh := ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-yt-sapisid-bbbb\n"

	out, err := prepareCookieImport(existing, netscapeHeader+fresh)
	if err != nil {
		t.Fatalf("prepareCookieImport: %v", err)
	}
	if !strings.Contains(out, "fake-google-sapisid-aaaa") {
		t.Error("the .google.com SAPISID row was evicted by a .youtube.com row of the same name — " +
			"the merge is keyed by name alone")
	}
	if !strings.Contains(out, "fake-yt-sapisid-bbbb") {
		t.Error("the pasted .youtube.com SAPISID did not replace the old one")
	}
	if strings.Contains(out, "fake-yt-sapisid-aaaa") {
		t.Error("both .youtube.com SAPISID rows survived — the file now carries two rows for one cookie")
	}
}

// TestPrepareCookieImportNeverWritesAnEmptyValuedRow covers BOTH filters, and
// the two are separate mutants.
//
//   - incoming: dropping the pre-merge filter lets an empty-valued SAPISID in a
//     paste win by name+domain over a working one on disk. The row then reads
//     as 6 fields to CookieJar.Load, which skips it: the credential is gone
//     from the jar and unprunable from the file.
//   - existing: dropping the post-merge filter carries a row an older writer
//     already left there straight back out, so the import cannot repair the
//     file it just rewrote. The fixture's stale row is HSID, a name the paste
//     does NOT carry, and that choice is the whole test: mergeCookieFiles keys a
//     7-field row by name+domain whatever its value, so an empty row the paste
//     shares a key with is REPLACED during the merge and the output filter is
//     never asked about it. Only a row that survives the merge catches this
//     mutant.
func TestPrepareCookieImportNeverWritesAnEmptyValuedRow(t *testing.T) {
	t.Run("an empty-valued paste row cannot evict a working one", func(t *testing.T) {
		existing := netscapeHeader + fakeYouTubeRows
		fresh := ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\t\n" +
			".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-bbbb\n"
		out, err := prepareCookieImport(existing, netscapeHeader+fresh)
		if err != nil {
			t.Fatalf("prepareCookieImport: %v", err)
		}
		jar := NewCookieJar()
		jar.loadFrom([]byte(out), "")
		if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "fake-sapisid-aaaa" {
			t.Errorf("SAPISID = %q, want the working existing value — an empty-valued paste row "+
				"overwrote a live credential", got)
		}
	})

	t.Run("an empty-valued row already on disk is not carried out", func(t *testing.T) {
		existing := netscapeHeader + fakeTwitchRows +
			".youtube.com\tTRUE\t/\tTRUE\t2000000000\tHSID\t\n"
		// HSID, because fakeYouTubeRows carries SAPISID and LOGIN_INFO and
		// nothing else: a stale row the paste would replace by name+domain never
		// reaches the output filter, and this subtest would stay green with that
		// filter deleted.
		if !strings.Contains(existing, "\tHSID\t\n") {
			t.Fatal("fixture is broken — the stale empty row must carry a name the paste does not")
		}
		out, err := prepareCookieImport(existing, netscapeHeader+fakeYouTubeRows)
		if err != nil {
			t.Fatalf("prepareCookieImport: %v", err)
		}
		for _, line := range strings.Split(out, "\n") {
			if isNetscapeDataRow(line) && netscapeRowValue(line) == "" {
				t.Errorf("the output still carries an empty-valued row: %q", line)
			}
		}
	})
}

// TestPrepareCookieImportNormalisesCRLF. Every browser extension on Windows
// exports CRLF. mergeCookieFiles carries a row VERBATIM, so without this the
// stray \r rides into cookies.txt on the end of the value field of every row
// but the last — where CookieJar.Load's TrimSpace hides it from this process
// and the next writer propagates it.
//
// The mutation: dropping the normalisation in cleanNetscapeRows.
func TestPrepareCookieImportNormalisesCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(netscapeHeader+fakeYouTubeRows, "\n", "\r\n")
	out, err := prepareCookieImport("", crlf)
	if err != nil {
		t.Fatalf("prepareCookieImport: %v", err)
	}
	if strings.Contains(out, "\r") {
		t.Errorf("the merged file carries a carriage return: %q", out)
	}
	jar := NewCookieJar()
	jar.loadFrom([]byte(out), "")
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "fake-sapisid-aaaa" {
		t.Errorf("SAPISID = %q — the CRLF export did not round-trip", got)
	}
}

// TestPrepareCookieImportOnAFirstAcquisition: there is no cookies.txt yet, and
// the result must still be a complete, loadable file rather than the bare rows.
//
// The mutation: `if existing == "" { return cleaned, nil }` — the written file
// then has no `# Netscape HTTP Cookie File` header. Nothing in Moombox needs
// it, which is exactly why that mutation survives every other test here; every
// other tool the operator might point at that file does.
func TestPrepareCookieImportOnAFirstAcquisition(t *testing.T) {
	out, err := prepareCookieImport("", netscapeHeader+fakeYouTubeRows+fakeTwitchRows)
	if err != nil {
		t.Fatalf("prepareCookieImport: %v", err)
	}
	if !strings.HasPrefix(out, "# Netscape HTTP Cookie File\n") {
		t.Error("a first import produced a file with no Netscape header")
	}
	jar := NewCookieJar()
	jar.loadFrom([]byte(out), "")
	if !jar.HasAnyYouTubeAuthCookie() || !jar.HasAnyTwitchAuthCookie() {
		t.Error("the first import did not produce a jar holding both platforms")
	}
}

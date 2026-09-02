# Arc 11 — Docker re-auth ingest Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give an operator with no shell access to the volume a browser-free way to replace dead YouTube/Twitch credentials — `POST /api/cookies/import`, a paste box and a file picker in the cookies settings panel, merging into `cookies.txt` and answering with the verdict of a live check.

**Architecture:** Four layers, in dependency order. (1) A PURE validator+merger in `internal/cookies` — parse the submitted Netscape text, refuse the three shapes that are not a usable export, and fold it into the existing file through `mergeCookieFiles` so a YouTube-only paste never destroys Twitch. (2) `AutoCookieService.ImportCookies` — the FIFTH writer of `cookies.txt`, doing read-through-the-seam then validate+merge then `writeFileAtomic` then `jar.Load` then verify, with Arc 2's abort rules inherited whole. (3) The route: session auth + CSRF + the `heavy` limiter + a size cap + two body shapes, ending in the wizard-finish's detached-and-flushed `CheckNow` so the credential write reaches the fingerprint comparison. (4) The UI, plus `FlagManualRelogin` restored with the caller that raises the prompt the import answers.

**Tech Stack:** Go 1.26, chi/v5, `net/http` multipart, vanilla JS + Shoelace v2.16 (Web UI), goja (for running the shipped JS modules in tests). No CGo. No new dependency.

**Spec:** `docs/superpowers/specs/2026-09-02-arc11-docker-ingest-design.md` — the plan argues from it; executors read both. The controller rulings it restates are in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/docker-ingest-brief.md` (gitignored, authoritative for the rulings), and the option analysis is `research-docker-cookie-acquisition.md` §1.7 / §2.B / §3.

## Global Constraints

- **SECURITY, verbatim:** No cookie value, token, login, or webhook URL may appear in any log line, notification, payload, error string, test name, or test fixture. Fixtures use obviously fake values like `fake-sapisid-aaaa`. Live gates take a cookie-file PATH via an environment variable and never print anything read from it.
- **`const livenessRecoveryArmed = false` at `internal/cookies/refresh.go` stays false.** Nothing in this arc arms the pilot.
- **`cmd/moombox/main.go:276-278` is a no-touch zone.** (G5's `SetExpectedPlatforms` gate.)
- **NEVER run two `go test ./...` at once.**
- **Gates, per task:** `go build ./...` · `go vet ./...` · `GOOS=linux go build ./...` · `gofmt -l internal/ cmd/` empty · `go test -count=1 ./...` reporting 27 `ok` lines / 0 fail from ONE run (`go list ./...` shows 31 packages; the other 4 carry no test files) · every file under `web/public` stays LF · `go build` after any `web/public` edit, because the assets are `go:embed`-ed and the goja tests read the EMBEDDED copy.
- **Standing test rule:** for EVERY assertion, this plan states the production mutation that breaks it. Bracketing an assertion to one function is no guard when a decoy sits inside it. Name checks and substring checks are no guard.
- **Every goroutine MUST have an inline `defer func() { if r := recover(); ... }()`.** Non-negotiable project rule. (This arc adds none — the detached re-check runs on the handler goroutine, which `RecoveryMiddleware` already covers.)
- **The logger is an anonymous interface repeated per struct** (`Debug/Info/Warn/Error(msg string, args ...any)`). Do NOT extract a named interface.
- **API routes use the `/api/` prefix, no version.** One new route: `POST /api/cookies/import`. **There is NO corresponding GET, ever** (controller ruling; spec R5).
- **No database change.** No new column, no schema bump, no `CookieStatus` member.
- **Do NOT bump the version or tag.** Release timing is the owner's call.
- **Commit messages end with these two trailer lines, exactly:**

```
Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
```

## File structure

| File | Task | Responsibility |
|---|---|---|
| `internal/cookies/errors.go` | 0 | Four new sentinels beside `ErrCookieFileUnreadable`: the three refusals and `ErrCookieFileUnwritable`. Sentinels live here, all of them, and nothing else does. |
| `internal/cookies/cookie_import.go` (new) | 0, 1 | The whole ingest: `prepareCookieImport` (pure) in the first half, `ImportResult` + `AutoCookieService.ImportCookies` (I/O) in the second. One file because the two change together and neither has another consumer. |
| `internal/cookies/autocookies_profile.go` | 1 | `credentialAccepted` — the acceptance predicate lifted out of `FinishSetupDetailed`'s closure so the import and the wizard cannot drift apart. |
| `internal/cookies/autocookies.go` | 2 | `FlagManualRelogin`, restored beside its neighbours. |
| `internal/web/routes/cookies.go` | 1 | `maxCookieImportBytes`, `readCookieImportBody`, `cookieImportOutcome`, and the `POST /api/cookies/import` handler on the `heavy` sub-router. |
| `cmd/moombox/monitor_callbacks.go` | 2 | The caller: a recovery that could not run raises the re-login prompt; the notification copy names the dashboard import. |
| `web/public/index.html` | 3 | The paste textarea, the file picker, the button and the inline result, inside the existing cookies panel. |
| `web/public/modules/settings.js` | 3 | `importCookies()` and `openCookieImport()`. |
| `web/public/modules/utils.js` | 3 | `reloginPromptTarget(status, hostname)` — the pure decision the two re-login click handlers share. |
| `web/public/app.js` | 3 | Both re-login click handlers route through that decision. |
| `README.md`, `docker/entrypoint.sh`, `SPEC.md`, `docs/spec/{operations,user-interfaces,data-and-storage}.md`, the Arc 10 plan's reload-site table | 4 | The sentences that stop being true. |
| `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md` | 5 | One new field gate. |

## Task list and model tier

| # | Title | Files | Tier |
|---|-------|-------|------|
| 0 | The validate-and-merge core, pure | `internal/cookies/cookie_import.go`, `errors.go`, `cookie_import_test.go` | sonnet |
| 1 | `ImportCookies` and the endpoint | `internal/cookies/cookie_import.go`, `autocookies_profile.go`, `internal/web/routes/cookies.go` + 3 test files | opus |
| 2 | `FlagManualRelogin`, restored with its caller | `internal/cookies/autocookies.go`, `cmd/moombox/monitor_callbacks.go` + 4 test files | sonnet |
| 3 | The settings panel and the re-login prompt's route to it | `web/public/{index.html,app.js,modules/settings.js,modules/utils.js}` + 1 test file | sonnet |
| 4 | The doc sentences that change with the code | `README.md`, `docker/entrypoint.sh`, `SPEC.md`, three `docs/spec/*.md`, the Arc 10 plan | sonnet |
| 5 | The live-gate decision and the field gate | `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md` | sonnet |

Tasks 0 → 1 → 2 → 3 are strictly ordered (each Consumes the previous). Task 4 depends on 0-3. Task 5 depends on 1 and 4.

Task 1 is opus because it is the security surface: an authenticated endpoint that accepts credential bytes, writes the highest-value secret in the app, and must not leak a byte of it back. Everything else has its code written out here.

---

### Task 0: The validate-and-merge core, pure

Spec R2 and R3, as one function with no I/O. Everything this task needs already exists in the package and must be REUSED rather than re-derived: `mergeCookieFiles` (`internal/cookies/autocookies_merge.go:142-211`) keys rows by name+domain and prunes expired ones; `netscapeCookiesHoldACredential` (`internal/cookies/autocookies_profile.go:356-382`) answers "is any of this a login" by loading the text into a THROWAWAY jar and asking the jar's own loose predicates, so the admission rules (`internal/cookies/jar.go:270-320`) and the auth-name lists (`youtubeAuthCookieNames` `jar.go:510-527`, `twitchAuthCookieNames` `jar.go:710`) have one reading in the package.

**Three refusals, and they are three because the operator's next move differs.** Not-Netscape means "re-export in the right format" (a JSON export from a cookie extension is the usual cause). No-rows means "your browser had nothing for this site". No-credential is THE common error and the only one that looks like success: an export taken from a signed-out window carries `YSC` and `VISITOR_INFO1_LIVE` and authenticates nothing.

**The empty-row trap is a FILTER RUN TWICE, and each run kills a different mutant.** `CookieJar.Load` `TrimSpace`s the line, so a trailing tab disappears, the row reads as 6 fields and is skipped — the credential vanishes from the jar while the row sits in the file forever, unprunable (`jar.go:223-243`, inside `loadFrom`, which is where the `TrimSpace` and the `< 7` skip actually are; `Load` itself is `:192-214`). Filtering the INCOMING text first is what stops an empty-valued `SAPISID` in a paste from overwriting a good one on disk (`mergeCookieFiles` lets the new row win by name+domain). Filtering the OUTPUT is what repairs rows an older writer already left there. Neither covers the other.

**Files:**
- Create: `internal/cookies/cookie_import.go`
- Modify: `internal/cookies/errors.go:190-192` (add four sentinels immediately after `ErrCookieFileUnreadable`, before `ErrAuthCheckNotAttempted`)
- Test: `internal/cookies/cookie_import_test.go` (create)
- Read (do not modify): `internal/cookies/autocookies_merge.go:142-228`, `internal/cookies/autocookies_profile.go:338-382`, `internal/cookies/jar.go:81-94` and `:270-320`

**Interfaces:**
- Consumes: `mergeCookieFiles(existing, newCookies string) string`; `netscapeCookiesHoldACredential(netscape string) bool`; `(*CookieJar).loadFrom(data []byte, filePath string)`; `(*CookieJar).GetCookieFor(p Platform, name string) string`
- Produces: `prepareCookieImport(existing, incoming string) (string, error)` — unexported, because the only caller is in this package and dead exported surface on a security-sensitive service reads to the next reader as a wired feature (the Arc 8 Task 12a lesson). Also `isNetscapeDataRow(line string) bool`, `netscapeRowValue(line string) string`, `stripEmptyValuedRows(netscape string) string`, `cleanNetscapeRows(incoming string) (cleaned string, kept, dropped, unparseable int)`, and the exported sentinels `ErrImportNotNetscape`, `ErrImportNoRows`, `ErrImportNoCredential`, `ErrCookieFileUnwritable` (Task 1's route maps these to status codes).

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/cookie_import_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestPrepareCookieImport' ./internal/cookies/`
Expected: FAIL to compile — `undefined: prepareCookieImport`, `undefined: ErrImportNotNetscape`, `undefined: isNetscapeDataRow`, `undefined: netscapeRowValue`.

- [ ] **Step 3: Add the four sentinels**

In `internal/cookies/errors.go`, immediately after the `ErrCookieFileUnreadable` block (`:190`) and before `ErrAuthCheckNotAttempted`:

```go
	// --- operator-supplied cookie import (POST /api/cookies/import) ---
	//
	// Three refusals rather than one, grouped by the same rule as the six
	// profile-import errors above: the operator's next move differs for each,
	// so collapsing them strips the only useful part of the message. Each says
	// what is wrong with the TEXT and never quotes any part of it — a cookie
	// NAME in a diagnostic is fine and useful, a value never is, and these
	// three are the only strings this endpoint composes about the submitted
	// bytes.

	// ErrImportNotNetscape is returned when NOTHING in the submitted text
	// parses as a cookie row: a JSON export from a browser extension, an HTML
	// error page, or some other format entirely. Distinct from ErrImportNoRows
	// because the remedy is "export it again, differently" rather than "sign in
	// first".
	ErrImportNotNetscape = errors.New("that is not a Netscape cookie file — no line in it is a tab-separated cookie row. " +
		"A JSON export from a cookie extension is the usual cause; re-export in Netscape (cookies.txt) format")

	// ErrImportNoRows is returned when the text IS a cookie file and carries no
	// data rows at all — the header and comments alone, which is what an export
	// from a browser holding nothing for the site produces.
	ErrImportNoRows = errors.New("that cookie file has no cookie rows — it holds only comments")

	// ErrImportNoCredential is returned when rows parsed and not one of them is
	// a YouTube or Twitch login cookie.
	//
	// THE common user error, and the only one that looks like success: an
	// export taken from a signed-out window is entirely well-formed and carries
	// YSC and VISITOR_INFO1_LIVE. Without this refusal it would be merged,
	// written, and discovered at the next members-only stream.
	ErrImportNoCredential = errors.New("that cookie file holds no YouTube or Twitch login cookie — " +
		"sign in to the site first, then export again from the window that is signed in")

	// ErrCookieFileUnwritable is returned when the merged file could not be
	// written. It exists so the import endpoint can name the ONE deployment
	// mistake that actually produces it — writeFileAtomic ends in a rename, and
	// a rename cannot replace a single-file bind mount — instead of answering a
	// bare 500. Wrapped ONLY on the import path; the other writers keep
	// returning the raw write error to their existing callers.
	ErrCookieFileUnwritable = errors.New("cookies.txt could not be written")
```

- [ ] **Step 4: Write the core**

Create `internal/cookies/cookie_import.go`:

```go
package cookies

import (
	"strings"
)

// prepareCookieImport validates an operator-supplied Netscape cookie file and
// returns the exact text that must be written to cookies.txt.
//
// PURE: no I/O, no clock of its own, no service state. Its caller
// (AutoCookieService.ImportCookies) owns the read, the write and the jar
// reload; everything decidable from the two strings is decided here, so the
// rules can be table-tested without a filesystem.
//
// MERGE, NEVER REPLACE (spec R3). A YouTube-only paste must leave every Twitch
// row exactly as it was: blind replacement would destroy the sibling platform's
// credentials silently, and the operator's next Twitch capture would fail for
// an unrelated-looking reason. mergeCookieFiles is the merge — name+domain
// keyed, verified tab-safe, and already the merge every other writer uses.
//
// The empty-value filter runs TWICE and both runs are load-bearing. See
// stripEmptyValuedRows for what an empty-valued row does to the jar.
func prepareCookieImport(existing, incoming string) (string, error) {
	cleaned, kept, dropped, unparseable := cleanNetscapeRows(incoming)
	switch {
	case kept == 0 && dropped == 0 && unparseable > 0:
		// Not one line looked like a cookie row.
		return "", ErrImportNotNetscape
	case kept == 0 && dropped == 0:
		// Comments and blanks only. An empty body never reaches here — the
		// route refuses that as a request-shape problem, which keeps this
		// message honest about what it describes.
		return "", ErrImportNoRows
	}

	// Rows that ARE structurally cookie rows but carry no login. Asked of the
	// throwaway jar rather than by matching names against the text, so the
	// domain routing, the name admission and the #HttpOnly_ handling have ONE
	// reading in this package. A paste that was all dropped rows lands here too
	// and gets this answer, correctly: it was a cookie file and none of it was
	// usable.
	if !netscapeCookiesHoldACredential(cleaned) {
		return "", ErrImportNoCredential
	}

	return stripEmptyValuedRows(mergeCookieFiles(existing, cleaned)), nil
}

// cleanNetscapeRows splits the submitted text into the data rows worth merging
// and counts what it threw away.
//
// The three counters are three because the caller's answer differs. `kept` and
// `dropped` both prove the text IS a cookie file; only `unparseable` on its own
// means it is not. Folding dropped into either neighbour would report a
// well-formed export whose values are blank as "not a Netscape file".
//
// CRLF is normalised here and nowhere else. Every browser extension on Windows
// exports CRLF, and mergeCookieFiles carries a row VERBATIM — so a stray \r
// would ride into cookies.txt on the end of a value, where CookieJar.Load's
// TrimSpace hides it from this process and the next writer propagates it.
func cleanNetscapeRows(incoming string) (cleaned string, kept, dropped, unparseable int) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(incoming, "\r\n", "\n"), "\r", "\n")
	var rows []string
	for line := range strings.SplitSeq(normalized, "\n") {
		if !isNetscapeDataRow(line) {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
		if len(fields) < 7 {
			unparseable++
			continue
		}
		if strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[5]) == "" || netscapeRowValue(line) == "" {
			dropped++
			continue
		}
		rows = append(rows, line)
		kept++
	}
	return strings.Join(rows, "\n"), kept, dropped, unparseable
}

// isNetscapeDataRow reports whether a line carries cookie data. `#HttpOnly_`
// lines are DATA despite the leading '#'; every other comment and every blank
// line is not. Same rule as CookieJar.loadFrom, countNetscapeCookieRows and
// mergeCookieFiles' own parser — a fourth reading of it is how they drift.
func isNetscapeDataRow(line string) bool {
	if strings.HasPrefix(line, "#HttpOnly_") {
		return true
	}
	if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
		return false
	}
	return true
}

// netscapeRowValue returns a data row's VALUE, or "" for a row too short to
// have one.
//
// Fields 6.. are re-JOINED, exactly as CookieJar.loadFrom does, so a value that
// legitimately contains a tab reads as itself rather than as a truncation.
func netscapeRowValue(line string) string {
	fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
	if len(fields) < 7 {
		return ""
	}
	return strings.TrimSpace(strings.Join(fields[6:], "\t"))
}

// stripEmptyValuedRows removes every data row whose value is empty, preserving
// comments, blanks and row order.
//
// THE TRAP, in full, because it is invisible from the file: CookieJar.Load
// TrimSpaces the line, so the trailing tab of `domain … NAME \t` disappears,
// the row reads as SIX fields and Load skips it. The credential is therefore
// absent from the jar while the row sits in cookies.txt forever — and no writer
// prunes it, because mergeCookieFiles prunes on EXPIRY and this row has a
// perfectly good expiry.
//
// Run on the OUTPUT, so an import repairs rows an older writer already left
// behind. cleanNetscapeRows runs the same rule on the INPUT, and that one is
// the guard that matters most: without it an empty-valued row in a paste wins
// by name+domain over a working credential on disk.
func stripEmptyValuedRows(netscape string) string {
	lines := strings.Split(netscape, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if isNetscapeDataRow(line) && netscapeRowValue(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestPrepareCookieImport' ./internal/cookies/`
Expected: PASS — 7 tests, subtests included, `ok github.com/vampiricwulf/Moombox/internal/cookies`.

- [ ] **Step 6: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: no `gofmt` output; 27 `ok` lines / 0 fail from that ONE run (31 packages listed, 4 with no test files).

- [ ] **Step 7: Commit**

```bash
git add internal/cookies/cookie_import.go internal/cookies/cookie_import_test.go internal/cookies/errors.go
git commit -F - <<'MSG'
feat(cookies): the validate-and-merge core for an operator-supplied cookie file

Arc 11 Task 0. prepareCookieImport is the pure half of the re-auth ingest. It
refuses the three shapes that are not a usable export -- text where no line is a
cookie row, a file of comments only, and the one that looks like success, an
export taken from a signed-out window -- and otherwise folds the paste into the
existing file through mergeCookieFiles, so a YouTube-only paste leaves every
Twitch row exactly as it was.

The empty-value filter runs twice and each run kills a different mutant. On the
INPUT it stops an empty-valued row in a paste winning by name+domain over a
working credential; on the OUTPUT it repairs rows an older writer already left
behind. Either way the row never reaches disk: Load TrimSpaces the line, the
trailing tab disappears, the row reads as six fields and is skipped, so the
credential is gone from the jar while the row sits in the file unprunable.

CRLF is normalised because mergeCookieFiles carries a row verbatim and every
Windows extension exports CRLF. The credential probe is the jar's own loose
predicates over a throwaway jar, so "is this a login" has one answer in the
package. Three refusals rather than one, because the operator's next move
differs for each; none of them quotes any part of the text.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---
### Task 1: `ImportCookies` and the endpoint

Spec R1, R3, R4, R5. Two halves in one task because they are one deliverable and one test cycle: the service method is the fifth writer of `cookies.txt`, and the route is the only thing that ever calls it.

**Arc 2's writer catalogue, inherited as a checklist rather than rediscovered.** The read goes through the `readCookieFile` seam (`autocookies.go:3318-3327`) and DISTINGUISHES "does not exist" from every other error: a transient read failure that falls through writes only the new cookies over a file that may hold the other platform's working credentials — reproduced end to end during Arc 2, with a Twitch token gone from disk while the run reported YouTube success. The write goes through `writeCookieFile` → `writeFileAtomic` (`autocookies.go:3312-3316`, `:3363`). Whatever renders `ErrCookieFileUnreadable` must NOT tell the operator to replace the file the endpoint just refused to touch (`errors.go:178-192`); that exact mistake shipped inside Arc 2 and had to be fixed before merge.

**The single-file bind-mount trap.** `writeFileAtomic` ends in a rename, and a rename cannot replace a bind-mounted FILE. `FinishSetupDetailed` already attaches a SHORT hint to its own failed write (`autocookies.go:1245-1255`) because that message goes to a status line both dashboards render; `refresh.go:2425-2429` attaches the long one because that message goes to a log. This endpoint's failure has to reach an HTTP response, so it takes the short sentence, through a sentinel the route can discriminate.

**The verdict comes from the import's OWN check, not from the re-check.** This is the wizard-finish shape exactly (`routes/cookies.go:460-567`): `FinishSetupDetailed` verifies and returns per-platform verdicts, the handler renders them, and the DETACHED `CheckNow` runs afterwards for the fingerprint comparison. Reading the verdict off `refreshSvc.GetStatus()` instead would put the answer downstream of a pass that has not run yet — the response would carry the PRE-import snapshot, which is the junction defect this subsystem keeps finding. The cost is honest and identical to the wizard's: two verify round-trips inside the request, two more in the detached re-check.

**Why the re-check is not optional and not `req.Context()`.** `refresh`'s status block is the only place the Twitch credential fingerprint is compared, the auth mark cleared and `OnCredentialsChanged` fired (Arc 10 R4/R5), and that block runs only inside a refresh pass. A client that closes the tab mid-pass would cancel both auth checks on a request-scoped context, `shouldObserveCredentials` bails on a check error, and the whole thing waits on the 30-minute ticker. The `Flush` is what makes "the client is not waiting on this" true: `jsonResponse` and `jsonError` both write into `net/http`'s bufio writer and neither flushes, and the handler does not return until the defer completes.

**Files:**
- Modify: `internal/cookies/cookie_import.go` (append `ImportResult` and `ImportCookies`)
- Modify: `internal/cookies/autocookies_profile.go:590-596` (add `credentialAccepted` beside `platformAuth`)
- Modify: `internal/cookies/autocookies.go:1295-1302` (replace the `accepted := func(...)` closure with a call, moving its comment to the new function)
- Modify: `internal/web/routes/cookies.go` — imports (`bytes`, `fmt`, `io`, `strings`), `maxCookieImportBytes` + `readCookieImportBody` + `cookieImportOutcome` above `CookieRoutes` (`:242`), and the handler immediately after the `/api/cookies/auto-refresh` block (`:420`) so the two credential-writing POSTs sit together on `heavy` (`:299-304`)
- Test: `internal/cookies/cookie_import_service_test.go` (create), `internal/web/routes/cookies_import_test.go` (create), `internal/web/routes/cookies_import_callsite_test.go` (create)
- Read (do not modify): `internal/web/routes/cookies.go:460-567` (the template), `internal/cookies/autocookies.go:2099-2131` (the read-abort shape), `internal/web/middleware.go:505-523` (`MaxBodySize`), `internal/web/rate_limiter.go:174-189`

**Interfaces:**
- Consumes (Task 0): `prepareCookieImport(existing, incoming string) (string, error)`; `ErrImportNotNetscape`, `ErrImportNoRows`, `ErrImportNoCredential`, `ErrCookieFileUnwritable`
- Consumes (existing): `readCookieFile`, `writeCookieFile`, `(*CookieJar).Load(path string) error`, `(*AutoCookieService).checkPlatformAuth(ctx) (yt, tw platformAuth)`, `verdictOf(p platformAuth) RefreshVerdict`, `(*AutoCookieService).setError(msg string)`, `(*RefreshService).CheckNow(ctx) bool`, `routeHandlerLit(t, file, route) *ast.FuncLit` (`cookies_browserread_callsite_test.go:74`), `nopRouteLogger` (`cookies_test.go:19`)
- Produces: `cookies.ImportResult{YouTube, Twitch RefreshVerdict; YouTubeAccepted, TwitchAccepted, Wrote bool}`; `(*cookies.AutoCookieService).ImportCookies(ctx context.Context, netscape string) (ImportResult, error)`; `credentialAccepted(p platformAuth) bool`; the route `POST /api/cookies/import`; `cookieImportOutcome(result cookies.ImportResult) map[string]any`; `const maxCookieImportBytes = 512 << 10`

- [ ] **Step 1: Write the failing service tests**

Create `internal/cookies/cookie_import_service_test.go`:

```go
package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// importService builds an AutoCookieService pointed at a real cookies.txt in a
// temp dir, with both verify callbacks answering `verified`. A real file
// because the whole point of this layer is the merge: a test against a stubbed
// writer is not testing that the sibling platform survived.
func importService(t *testing.T, seed string, verified bool) (*AutoCookieService, string, *CookieJar) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	s := NewAutoCookieService(dir, path, jar, nopAutoCookieLogger{})
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return verified, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return verified, nil }
	return s, path, jar
}

// TestImportCookiesMergesOntoDiskAndReloadsTheJar is the end-to-end claim of
// this layer, asserted on the FILE and on the live jar rather than on the
// returned struct.
//
// The mutations: writing `netscape` instead of the merged text (Twitch
// disappears from the file); dropping the jar.Load (the process keeps serving
// the dead credentials until the 30-minute ticker, which is exactly what
// "immediately apply the updated cookie" rules out).
func TestImportCookiesMergesOntoDiskAndReloadsTheJar(t *testing.T) {
	s, path, jar := importService(t, netscapeHeader+fakeTwitchRows, true)

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if !result.Wrote {
		t.Fatal("Wrote is false after a successful import — the caller's re-check is gated on it")
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back cookies.txt: %v", err)
	}
	if !strings.Contains(string(onDisk), "fake-authtoken-aaaa") {
		t.Error("the Twitch row is gone from cookies.txt — the import replaced instead of merging")
	}
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the pasted YouTube row never reached the file")
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "fake-sapisid-aaaa" {
		t.Errorf("the live jar's SAPISID = %q — the jar was not reloaded from the file just written", got)
	}
	if result.YouTube != RefreshOK || !result.YouTubeAccepted {
		t.Errorf("YouTube verdict = %v accepted = %v, want ok/true from a verify that answered true",
			result.YouTube, result.YouTubeAccepted)
	}
}

// TestImportCookiesAbortsOnAnUnreadableExistingFile is Arc 2's S9, at the fifth
// writer.
//
// The mutation: treating a non-ENOENT read error as "no existing file". The
// import then writes ONLY the pasted rows over a file that may hold the other
// platform's working credentials — reproduced end to end during Arc 2 — and the
// operator is told to replace the file that was never the problem.
func TestImportCookiesAbortsOnAnUnreadableExistingFile(t *testing.T) {
	s, path, _ := importService(t, netscapeHeader+fakeTwitchRows, true)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	blip := errors.New("simulated read failure")
	restore := readCookieFile
	readCookieFile = func(string) ([]byte, error) { return nil, blip }
	t.Cleanup(func() { readCookieFile = restore })

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Fatalf("error = %v, want it to wrap ErrCookieFileUnreadable", err)
	}
	if result.Wrote {
		t.Error("Wrote is true on the abort — the caller would run a re-check over a file nobody touched")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(before) {
		t.Error("cookies.txt changed on the read-abort path — the abort exists precisely to leave it alone")
	}
	if !strings.Contains(err.Error(), blip.Error()) {
		t.Errorf("the abort drops the underlying cause: %v", err)
	}
}

// TestImportCookiesNamesTheBindMountOnAFailedWrite. writeFileAtomic ends in a
// rename and a rename cannot replace a single-file bind mount, which is the one
// deployment mistake that produces this in the deployment this endpoint exists
// for.
//
// The mutations: returning the bare write error (the route's default arm then
// answers "cookie import failed", which names nothing the operator can act on);
// setting Wrote before the write succeeds.
func TestImportCookiesNamesTheBindMountOnAFailedWrite(t *testing.T) {
	s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)

	restore := writeCookieFile
	writeCookieFile = func(string, []byte, os.FileMode) error {
		return errors.New("rename temp cookie file: device or resource busy")
	}
	t.Cleanup(func() { writeCookieFile = restore })

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if !errors.Is(err, ErrCookieFileUnwritable) {
		t.Fatalf("error = %v, want it to wrap ErrCookieFileUnwritable", err)
	}
	if result.Wrote {
		t.Error("Wrote is true although the write failed")
	}
	if !strings.Contains(err.Error(), "Docker") {
		t.Errorf("the failed-write message does not name the bind-mount mistake: %v", err)
	}
}

// TestImportCookiesRecordsOnlyTheFailuresItEstablished is the lastError write
// policy at this writer (data-and-storage.md § Auto-Cookie Service).
//
// lastErrorSnapshot is this package's EXISTING test helper
// (autocookies_periodic_start_test.go:48) — do not add a second one. It reads
// the field under the lock its writers hold, which is exactly what
// AutoCookieStatus.LastError projects, and it avoids GetStatus's
// browser/registry detection scan (filesystem I/O and a reg.exe spawn on
// Windows) that the rest of this package neutralises with
// withFreshBrowserDetectCache + stubDetectors.
//
// A REJECTED PASTE is not a state of the install: nothing ran, nothing was
// written, and the caller gets the answer synchronously in the same dialog —
// the same reasoning that keeps FinishSetupDetailed's two guard clauses from
// setting. A read or write failure IS a state of the install, and Settings is
// where an operator looks afterwards.
//
// The mutations: adding a setError to the validation exit (every mistyped
// paste leaves a red line in Settings that no later success clears); removing
// it from the read-abort exit (the abort becomes invisible the moment the
// response is closed).
func TestImportCookiesRecordsOnlyTheFailuresItEstablished(t *testing.T) {
	t.Run("a rejected paste records nothing", func(t *testing.T) {
		s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)
		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeSignedOutRows); !errors.Is(err, ErrImportNoCredential) {
			t.Fatalf("error = %v, want ErrImportNoCredential", err)
		}
		if got := lastErrorSnapshot(s); got != "" {
			t.Errorf("lastError = %q after a rejected paste — a bad paste is not a state of the install", got)
		}
	})

	t.Run("a read abort records itself", func(t *testing.T) {
		s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)
		restore := readCookieFile
		readCookieFile = func(string) ([]byte, error) { return nil, errors.New("simulated read failure") }
		t.Cleanup(func() { readCookieFile = restore })

		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err == nil {
			t.Fatal("expected the read abort")
		}
		got := lastErrorSnapshot(s)
		if !strings.Contains(got, ErrCookieFileUnreadable.Error()) {
			t.Errorf("lastError = %q, want the abort's own message", got)
		}
	})
}

// TestImportCookiesClearsTheReloginFlagForAnAcceptedPlatform.
//
// The flag means "go and sign in again"; the operator just did exactly that, by
// the one route a container has. Leaving it raised because the confirming
// request hit a rate limit would nag them about work already done — which is
// why this clears on ACCEPTED, not on verified, exactly as FinishSetupDetailed
// does. The needsRelogin map is written directly here because the exported
// setter arrives in Task 2; that task swaps this fixture for the setter and
// adds the raise side.
//
// The mutations: clearing on `result.YouTube == RefreshOK` (an operator whose
// verify was rate-limited keeps a red re-login badge over a good import);
// clearing both platforms whatever the paste carried (a YouTube-only paste
// silently retracts a live Twitch alarm).
func TestImportCookiesClearsTheReloginFlagForAnAcceptedPlatform(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, errors.New("rate limited") }
	s.mu.Lock()
	s.needsRelogin["youtube"] = true
	s.needsRelogin["twitch"] = true
	s.mu.Unlock()

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}

	relogin := s.ReloginStatus()
	if relogin["youtube"] {
		t.Error("the YouTube re-login flag is still raised after an accepted import — an inconclusive " +
			"check is not evidence against a sign-in the operator just supplied")
	}
	if !relogin["twitch"] {
		t.Error("the Twitch re-login flag was cleared by a YouTube-only paste — nothing in that paste " +
			"says anything about Twitch")
	}
}

// argRecordingLogger keeps the ARGS as well as the message, which is what a leak
// scan has to read. Neither existing recorder in this package does:
// recordingCookieLogger (autocookies_periodic_start_test.go:18) discards them in
// its signature, and capturingLogger keeps messages only. A third recorder,
// narrowly, rather than widening one of those and changing what its own tests
// compare.
//
// The anonymous four-method interface, repeated — never extracted.
type argRecordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *argRecordingLogger) record(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(args...))
}
func (l *argRecordingLogger) Debug(msg string, args ...any) { l.record(msg, args...) }
func (l *argRecordingLogger) Info(msg string, args ...any)  { l.record(msg, args...) }
func (l *argRecordingLogger) Warn(msg string, args ...any)  { l.record(msg, args...) }
func (l *argRecordingLogger) Error(msg string, args ...any) { l.record(msg, args...) }
func (l *argRecordingLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// importServiceWithLog is importService with a logger that keeps what it was
// handed, for the scans below. Verification always answers true; none of these
// subtests gets that far.
func importServiceWithLog(t *testing.T, seed string) (*AutoCookieService, *argRecordingLogger) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	log := &argRecordingLogger{}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	s := NewAutoCookieService(dir, path, jar, log)
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	return s, log
}

// TestImportFailurePathsCarryNoValue extends the security rule to the three
// answers the ROUTE's leak scan cannot reach from outside this package, because
// reaching them needs the read and write seams: the S9 abort, the failed write,
// and a write that landed over a jar that then could not read it back.
//
// What is asserted is narrow and exact. All three wrap an OS error, and an OS
// error carries a PATH — that is not a secret, and the abort's message is
// useless without it. The claim is that no cookie VALUE rides along with it: not
// in the returned error, not in lastError (which both dashboards render), and
// not in any log line.
//
// The mutation: any of the three exits reworded to interpolate the paste, the
// merged text, or a row of either — the shape a "helpful" diagnostic naming the
// offending row would take.
func TestImportFailurePathsCarryNoValue(t *testing.T) {
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa"}
	paste := netscapeHeader + fakeYouTubeRows

	scan := func(t *testing.T, err error, log *argRecordingLogger, s *AutoCookieService) {
		t.Helper()
		if err == nil {
			t.Fatal("expected this path to fail")
		}
		haystack := err.Error() + "\n" + lastErrorSnapshot(s) + "\n" + log.all()
		for _, secret := range secrets {
			if strings.Contains(haystack, secret) {
				t.Fatalf("a cookie value reached an error, lastError or a log line: %q", secret)
			}
		}
	}

	t.Run("the read abort", func(t *testing.T) {
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := readCookieFile
		readCookieFile = func(string) ([]byte, error) { return nil, errors.New("simulated read failure") }
		t.Cleanup(func() { readCookieFile = restore })

		_, err := s.ImportCookies(context.Background(), paste)
		scan(t, err, log, s)
	})

	t.Run("the failed write", func(t *testing.T) {
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := writeCookieFile
		writeCookieFile = func(string, []byte, os.FileMode) error {
			return errors.New("rename temp cookie file: device or resource busy")
		}
		t.Cleanup(func() { writeCookieFile = restore })

		_, err := s.ImportCookies(context.Background(), paste)
		scan(t, err, log, s)
	})

	t.Run("the write landed and the jar could not read it back", func(t *testing.T) {
		// A DIRECTORY where the file belongs: the stubbed write reports success
		// and jar.Load then fails on that same path on both platforms (EISDIR on
		// Linux, a read error on Windows). It is the one error exit with
		// Wrote == true, and the one the route answers with its own fixed
		// sentence.
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := writeCookieFile
		writeCookieFile = func(path string, _ []byte, _ os.FileMode) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return os.Mkdir(path, 0o755)
		}
		t.Cleanup(func() { writeCookieFile = restore })

		result, err := s.ImportCookies(context.Background(), paste)
		if !result.Wrote {
			t.Fatalf("Wrote = false on the reload failure — the route's re-check is gated on it (err = %v)", err)
		}
		scan(t, err, log, s)
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestImportCookies|TestImportFailurePaths' ./internal/cookies/`
Expected: FAIL to compile — `s.ImportCookies undefined (type *AutoCookieService has no field or method ImportCookies)`.

- [ ] **Step 3: Lift the acceptance predicate out of the wizard's closure**

In `internal/cookies/autocookies_profile.go`, immediately after `func (p platformAuth) ok() bool` (`:596`):

```go
// credentialAccepted is what the CALLER is told about one platform, and it is
// deliberately not the verification result.
//
// A sign-in the user just completed is accepted when the site could not answer:
// a 429, a captive portal or a DNS blip is not evidence against a login that
// happened thirty seconds ago, and refusing it would send the user back through
// a wizard that was working. False failure is the worse direction there.
//
// It is NOT accepted when nothing was ever asked. A jar that cannot produce a
// cookie header or a SAPISIDHASH made no request, so there is no answer to
// extend the benefit of the doubt to — and this value is what setup.js turns
// into a green "YouTube cookies configured" badge and an entry in
// active_platforms. Because both producers MERGE the pre-existing cookies.txt
// before checking, a leftover Google remnant with no SAPISID would otherwise
// light that badge up for a user who only signed in to Twitch. `attempted` is
// what separates the two; see platformAuth.
//
// TWO producers now — FinishSetupDetailed and ImportCookies — which is why this
// is a function rather than the closure it was: the wizard and the import
// answer the same question about the same states, and two copies of a
// three-clause predicate is how they come to disagree about the middle one.
func credentialAccepted(p platformAuth) bool {
	return p.hasCookies && (p.state == verifyOK || (p.state == verifyUnknown && p.attempted))
}
```

In `internal/cookies/autocookies.go`, delete the `accepted := func(p platformAuth) bool {...}` closure (`:1295-1302`), move the comment above it into the function you just wrote, and replace the two call sites:

```go
	ytAuth := credentialAccepted(ytCheck)
	twAuth := credentialAccepted(twCheck)
```

- [ ] **Step 4: Write `ImportResult` and `ImportCookies`**

Append to `internal/cookies/cookie_import.go` (and extend the import block to `context`, `errors`, `fmt`, `io/fs`, `os`, `path/filepath`, `strings`):

```go
// ImportResult reports what ONE operator-supplied cookie import concluded, per
// platform.
//
// Field for field this is SetupResult's shape, and deliberately so: an import
// is an acquisition with a per-platform verdict, exactly like a wizard finish,
// and cookieImportOutcome renders the same key set cookieSetupOutcome does so
// the dashboard reuses the copy helpers rather than inventing a fourth phrasing
// of the same three states. It is a distinct TYPE because SetupResult's doc
// says what one INTERACTIVE SETUP concluded, and an import is not a setup —
// there is no browser, no slot and no cancel.
//
// The two facts per platform can disagree, which is the whole reason both are
// here: see credentialAccepted for why an inconclusive check still accepts a
// credential the operator just supplied.
type ImportResult struct {
	// What the auth check CONCLUDED, in the three-way vocabulary a refresh pass
	// uses. RefreshUnknown is the zero value on purpose, so any exit that
	// returns without checking cannot accidentally assert health or failure.
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	YouTubeAccepted bool
	TwitchAccepted  bool

	// Wrote reports that this import REPLACED cookies.txt, and it is true on
	// one error path as well as on success — the jar reload after a successful
	// write can fail, and that exit returns an error over a file that has
	// already been replaced. Its caller is the route's deferred re-check, which
	// must fire on exactly that case: it is where a re-check is worth most,
	// because refresh's own jar.Reload repairs the stale in-memory jar the
	// error left behind. Same contract, same caller, as SetupResult.Wrote.
	Wrote bool
}

// ImportCookies merges an operator-supplied Netscape cookie file into
// cookies.txt, reloads the jar and verifies what it just installed.
//
// THE FIFTH WRITER of cookies.txt, and it inherits Arc 2's catalogue whole: the
// read goes through the readCookieFile seam and distinguishes "does not exist"
// from every other error, the merge goes through mergeCookieFiles, the write
// goes through writeCookieFile, and no empty-valued row is ever produced (see
// prepareCookieImport).
//
// NOT gated on the `stopped` latch, unlike StartSetup and
// RefreshCookiesDetailed. Both of those refusals are about launching or
// steering a browser PROCESS; this launches nothing, and refusing an import
// during a drain would throw away credentials the operator supplied by hand,
// for the sake of a shutdown that is about to read the file back on the next
// start anyway.
//
// The caller runs the auth re-check. Every gesture that can write cookies.txt
// must end in one (Arc 10 R4) and this one's caller lives in
// internal/web/routes, which — like the two setup-wizard finishes — runs it
// itself rather than through the OnPassCompleted seam. Firing that seam here
// would double every external site; see its doc comment.
func (s *AutoCookieService) ImportCookies(ctx context.Context, netscape string) (ImportResult, error) {
	if s.cookiePath == "" {
		// Unreachable in production — cmd/moombox always constructs the service
		// with cookies.cookie_file, which config defaults to ./cookies.txt —
		// but writing to "" would otherwise produce a temp file in the process
		// working directory and a rename onto nothing.
		return ImportResult{}, errors.New("no cookie file is configured — set cookies.cookie_file")
	}

	var existing string
	data, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		existing = string(data)
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — first acquisition. Nothing to merge, nothing to
		// protect.
	default:
		// Abort BEFORE anything is written. A transient read failure is NOT
		// "no existing file": the unreadable file may hold working credentials
		// for a platform this paste never mentions, and proceeding as if it
		// were absent would replace it with the paste alone.
		//
		// Wrapped so the route can tell this apart from every other import
		// failure — the operator must be told to fix the permission or the
		// mount, and NEVER to replace the file Moombox just went out of its way
		// not to destroy.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.setError(mergeErr.Error())
		s.logger.Error("cookie import: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		return ImportResult{}, mergeErr
	}

	// No setError on this exit, deliberately, and it is the same rule that
	// keeps FinishSetupDetailed's two guard clauses from setting: a rejected
	// paste is not a state of the INSTALL. Nothing ran, nothing was written,
	// and the caller renders the refusal synchronously in the dialog that
	// produced it — while lastError renders in Settings as a standing "your
	// recordings will fail" that no later success clears.
	merged, err := prepareCookieImport(existing, netscape)
	if err != nil {
		return ImportResult{}, err
	}

	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		s.setError("could not create the directory for cookies.txt: " + err.Error())
		return ImportResult{}, err
	}
	if err := writeCookieFile(s.cookiePath, []byte(merged), 0o600); err != nil {
		// The hint names the ONE deployment mistake that produces this, and is
		// kept SHORT because it goes to a status line both dashboards render —
		// the same split FinishSetupDetailed's failed write already makes
		// against refresh.go's log-bound paragraph.
		wrErr := fmt.Errorf("%w (%w) — if this is Docker, mount the data directory rather than cookies.txt itself",
			ErrCookieFileUnwritable, err)
		s.setError(wrErr.Error())
		return ImportResult{}, wrErr
	}

	// Wrote is set from here down: the file on disk has been replaced, so every
	// exit past this point leaves a credential pair the running process may not
	// have compared yet.
	result := ImportResult{Wrote: true}

	// Load(path), not Reload(): Reload re-reads the jar's OWN filePath and is a
	// silent no-op on a jar that was never loaded from one, which would leave
	// the process serving the old credentials while the file on disk is
	// correct. Load is what the other two merge-writers use.
	if err := s.jar.Load(s.cookiePath); err != nil {
		s.setError("the imported cookies were written but could not be loaded: " + err.Error())
		return result, err
	}

	yt, tw := s.checkPlatformAuth(ctx)
	result.YouTube = verdictOf(yt)
	result.Twitch = verdictOf(tw)
	result.YouTubeAccepted = credentialAccepted(yt)
	result.TwitchAccepted = credentialAccepted(tw)

	// Clear the re-login flag for every platform this import ACCEPTED, not just
	// the ones it verified — the operator has just done the thing the flag asks
	// for, and leaving it raised because the confirming request hit a rate
	// limit would nag them about work already done. Process-local; the next
	// conclusive check re-raises it. Same rule, same wording, as
	// FinishSetupDetailed's clear.
	s.mu.Lock()
	if result.YouTubeAccepted {
		s.needsRelogin["youtube"] = false
	}
	if result.TwitchAccepted {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	return result, nil
}
```

- [ ] **Step 5: Run the service tests to verify they pass**

Run: `go test -count=1 -run 'TestImportCookies|TestImportFailurePaths|TestPrepareCookieImport|TestFinishSetup' ./internal/cookies/`
Expected: PASS — the five new `TestImportCookies*` plus `TestImportFailurePathsCarryNoValue`, Task 0's seven, and every existing `TestFinishSetup*` still green (the closure lift changed no behaviour).

- [ ] **Step 6: Write the failing route tests**

Create `internal/web/routes/cookies_import_test.go`:

```go
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// Fixtures. Obviously fake values, far-future expiries (mergeCookieFiles prunes
// on expiry), and every one of them is scanned for in the leak test below.
const (
	importHeader     = "# Netscape HTTP Cookie File\n"
	importYouTube    = ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-sapisid-aaaa\n"
	importYouTubeTwo = ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-aaaa\n"
	importTwitch     = ".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\tfake-authtoken-aaaa\n"
	importSignedOut  = ".youtube.com\tTRUE\t/\tFALSE\t2000000000\tYSC\tfake-ysc-aaaa\n"
)

func importPaste() string { return importHeader + importYouTube + importYouTubeTwo }

// recordingLogger captures every line the service logs, so the leak scan can
// read them. The anonymous four-method interface, repeated — never extracted.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) record(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(args...))
}
func (l *recordingLogger) Debug(msg string, args ...any) { l.record(msg, args...) }
func (l *recordingLogger) Info(msg string, args ...any)  { l.record(msg, args...) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.record(msg, args...) }
func (l *recordingLogger) Error(msg string, args ...any) { l.record(msg, args...) }
func (l *recordingLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// importRouter wires the real CookieRoutes over a service pointed at a real
// cookies.txt. The RefreshService is real too and its jar is EMPTY, so the
// handler's deferred CheckNow short-circuits before any network call.
func importRouter(t *testing.T, seed string, rl *web.RateLimiter) (chi.Router, string, *recordingLogger, *cookies.AutoCookieService) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	log := &recordingLogger{}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	svc := cookies.NewAutoCookieService(dir, path, jar, log)
	// Verified without a network call: the callbacks are the seam
	// checkPlatformAuth goes through, and a nil callback would report success
	// on presence alone, which is a different state.
	svc.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	svc.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	r := chi.NewRouter()
	CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, rl)
	return r, path, log, svc
}

func postImport(t *testing.T, r chi.Router, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestImportRouteWritesAndAnswersWithTheVerdict — the happy path, asserted on
// the FILE (the sibling platform survived) and on the wire (the verdict).
//
// The mutation: answering before calling ImportCookies, or rendering
// refreshSvc.GetStatus() instead of the result — the response would then carry
// the PRE-import snapshot, and a bad export would be reported as fine.
func TestImportRouteWritesAndAnswersWithTheVerdict(t *testing.T) {
	r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)

	rec := postImport(t, r, "text/plain", importPaste())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	if body["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", body["authenticated"])
	}
	if body["youtubeVerification"] != "ok" {
		t.Errorf("youtubeVerification = %v, want \"ok\"", body["youtubeVerification"])
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(onDisk), "fake-authtoken-aaaa") {
		t.Error("the Twitch row is gone — the endpoint replaced rather than merged")
	}
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the pasted YouTube row never reached the file")
	}
}

// TestImportOutcomeSpeaksTheSetupOutcomeVocabulary. The two payloads answer the
// same question about the same three states, and the dashboard renders both
// through cookieSetupAcceptedToast / cookieSetupRejectedMessage. A key added to
// one and not the other is the junction defect this file keeps finding: the
// import's UI silently reads `undefined` and hedges about a working session.
//
// The mutation: renaming or adding a key in either renderer.
func TestImportOutcomeSpeaksTheSetupOutcomeVocabulary(t *testing.T) {
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	got := keys(cookieImportOutcome(cookies.ImportResult{}))
	want := keys(cookieSetupOutcome(cookies.SetupResult{}))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cookieImportOutcome keys = %v, cookieSetupOutcome keys = %v — the two must stay "+
			"identical so the dashboard's copy helpers read both", got, want)
	}
}

// TestImportRouteRefusesTheThreeShapesWithTheirOwnMessage.
//
// The mutation: collapsing the three sentinels into the default arm. Every bad
// paste then answers "cookie import failed" with a 500, which names nothing the
// operator can act on and reads as a server fault rather than a bad export.
func TestImportRouteRefusesTheThreeShapesWithTheirOwnMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"json export", `[{"name":"SAPISID"}]`, cookies.ErrImportNotNetscape},
		{"comments only", importHeader + "# nothing here\n", cookies.ErrImportNoRows},
		{"signed-out export", importHeader + importSignedOut, cookies.ErrImportNoCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)
			before, _ := os.ReadFile(path)

			rec := postImport(t, r, "text/plain", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %q", rec.Body.String())
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want.Error()) {
				t.Errorf("error = %q, want it to carry %q", msg, tc.want.Error())
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Error("a refused paste still rewrote cookies.txt")
			}
		})
	}
}

// TestImportRouteRefusesTheWrongRequestShapes. Each of these is a distinct
// operator mistake with a distinct answer, and each has its own mutant: drop
// the cap and a 50 MB paste is merged into memory and onto disk; drop the
// content-type switch and a JSON body posted by a well-meaning client is read
// as cookie text and rejected with the wrong sentence; drop the empty-body
// check and a mis-click reports "that cookie file has no cookie rows" about a
// file the operator never sent.
func TestImportRouteRefusesTheWrongRequestShapes(t *testing.T) {
	t.Run("over the cap, pasted", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		oversize := importPaste() + strings.Repeat("# padding\n", 60000)
		rec := postImport(t, r, "text/plain", oversize)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body.String())
		}
	})

	// The two shapes reach the cap through DIFFERENT code, so one subtest does
	// not cover both: the paste gets the *http.MaxBytesError straight back from
	// io.ReadAll, while the upload gets whatever ParseMultipartForm returns for a
	// body whose reader refused. errors.As has to find the sentinel through
	// that, or an oversize upload answers 400 "no `cookies` file part" — a
	// sentence about the wrong mistake, on the shape a phone uses.
	t.Run("over the cap, uploaded", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("cookies", "cookies.txt")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write([]byte(importPaste() + strings.Repeat("# padding\n", 60000))); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close multipart: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413 — a 400 here means ParseMultipartForm wrapped the cap error "+
				"somewhere errors.As cannot reach, and the reader needs its own check: %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("an unsupported content type", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		rec := postImport(t, r, "application/json", `{"cookies":"..."}`)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status %d, want 415: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an empty body", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		rec := postImport(t, r, "text/plain", "   \n\n")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestImportRouteAcceptsAMultipartUpload — the file picker's shape. The panel
// has two controls and this is the one a phone uses; without it the file input
// posts a body the handler answers 415 to.
//
// The mutation: deleting the multipart arm, or reading the wrong part name.
func TestImportRouteAcceptsAMultipartUpload(t *testing.T) {
	r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("cookies", "cookies.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte(importPaste())); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	onDisk, _ := os.ReadFile(path)
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the uploaded rows never reached the file")
	}
}

// TestImportRouteIsOnTheHeavyLimiter. The endpoint validates, merges, writes and
// then makes up to four live auth round-trips; unlimited it is a free
// amplifier against two upstreams and a rewrite of the credential file per
// request.
//
// The mutation: registering the route on `r` instead of `heavy`.
func TestImportRouteIsOnTheHeavyLimiter(t *testing.T) {
	rl := web.NewRateLimiterCtx(context.Background(), 1, time.Minute)
	t.Cleanup(rl.Close)
	r, _, _, _ := importRouter(t, "", rl)

	if rec := postImport(t, r, "text/plain", importPaste()); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec := postImport(t, r, "text/plain", importPaste())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429 — the route is not on the heavy limiter", rec.Code)
	}
}

// TestImportRouteHasNoGET is the third controller ruling, at the wire. The
// endpoint accepts credential bytes and never serves them; that asymmetry is
// the whole of what keeps it from being an exfiltration path.
//
// The mutation: adding a GET handler for this path — "so the panel can show
// what is currently loaded" is exactly how it would arrive.
func TestImportRouteHasNoGET(t *testing.T) {
	r, _, _, _ := importRouter(t, importHeader+importYouTube+importYouTubeTwo, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cookies/import", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/cookies/import answered %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "fake-sapisid-aaaa") {
		t.Fatal("GET /api/cookies/import served a cookie value")
	}
}

// importRouterOverADirectory points the service's cookie path at a DIRECTORY,
// so the pre-merge read fails with something other than "does not exist" — the
// S9 abort — on both platforms (EISDIR on Linux, a read error on Windows) and
// without the unexported seam, which this package cannot reach.
//
// It exists for the leak scan below: that abort's message is the one route
// answer that interpolates an OS error, and an OS error carries a path.
func importRouterOverADirectory(t *testing.T) (chi.Router, *recordingLogger) {
	t.Helper()
	dir := t.TempDir()
	blocked := filepath.Join(dir, "cookies.txt")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir the blocking directory: %v", err)
	}
	log := &recordingLogger{}
	svc := cookies.NewAutoCookieService(dir, blocked, cookies.NewCookieJar(), log)
	r := chi.NewRouter()
	CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, nil)
	return r, log
}

// TestImportRouteLeaksNoValueAnywhere is the security rule, executed: drive
// every answer this endpoint can give with a body full of distinctive fake
// values, and scan the response AND every log line for each of them.
//
// THE ANSWERS THAT WRAP AN OS ERROR are included, and they are the interesting
// ones. The three sentinels are fixed strings and cannot carry anything; the
// unreadable-file abort interpolates `readErr` and logs the path, so a path DOES
// appear — which is intended, the message is useless without it, and a path is
// not a secret. What this proves about that answer is the narrower claim: no
// cookie VALUE rides along with it.
//
// The unwritable-file and reload-failure answers cannot be reached from this
// package (they need the write seam), and they are scanned in
// TestImportFailurePathsCarryNoValue over in internal/cookies. The reload
// failure's route arm is additionally value-free by construction: it interpolates
// nothing at all.
//
// The mutation: any handler or sentinel that grows a %s of the submitted text —
// the shape a "helpful" diagnostic naming the offending row would take.
func TestImportRouteLeaksNoValueAnywhere(t *testing.T) {
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa", "fake-ysc-aaaa"}
	scan := func(t *testing.T, rec *httptest.ResponseRecorder, log *recordingLogger) {
		t.Helper()
		haystack := rec.Body.String() + "\n" + fmt.Sprint(rec.Header()) + "\n" + log.all()
		for _, secret := range secrets {
			if strings.Contains(haystack, secret) {
				t.Fatalf("a cookie value reached the response or the log: %q", secret)
			}
		}
	}

	for _, tc := range []struct{ name, body string }{
		{"accepted", importPaste()},
		{"signed-out", importHeader + importSignedOut},
		{"not netscape", `[{"name":"SAPISID","value":"fake-sapisid-aaaa"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, log, _ := importRouter(t, importHeader+importTwitch, nil)
			rec := postImport(t, r, "text/plain", tc.body)
			scan(t, rec, log)
		})
	}

	t.Run("the unreadable-file abort", func(t *testing.T) {
		r, log := importRouterOverADirectory(t)
		rec := postImport(t, r, "text/plain", importPaste())
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422 — an unreadable cookies.txt must reach the operator as the "+
				"S9 abort, not as a bare 500: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "refusing to merge or overwrite") {
			t.Errorf("the 422 does not carry the abort's own sentence, so nothing tells the operator "+
				"the file was left alone: %s", rec.Body.String())
		}
		scan(t, rec, log)
	})
}
```

- [ ] **Step 7: Write the failing call-site test**

Create `internal/web/routes/cookies_import_callsite_test.go`:

```go
package routes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestImportHandlerEndsInADetachedFlushedRecheck.
//
// Arc 10 R4: refresh's status block is the ONLY place the Twitch credential
// fingerprint is compared, the auth mark cleared and OnCredentialsChanged
// fired, and that block runs only inside a refresh pass — so every gesture that
// can put new credentials on disk has to reach CheckNow, or the repair waits on
// the 30-minute ticker. This endpoint is the newest such gesture and the one
// built specifically for the deployment with no other repair path.
//
// Structural, and this is the strongest assertion available here for the same
// reason the wizard-finish site has none: driving a real CheckNow from this
// package means a live guide POST and a live oauth2/validate, because
// youtubeGuideURL and twitchValidateURL are unexported package vars in
// internal/cookies. The behavioural half is pinned inside that package
// (TestCheckNowObservesATwitchCredentialChange) over the same public entry
// point; what can only be asserted HERE is that this handler calls it.
//
// THE FOUR MUTANTS, each its own assertion:
//
//   - delete the defer: no CheckNow at all. The most likely regression, because
//     everything else about the endpoint still works.
//   - hand CheckNow the REQUEST's context, in any of its three spellings: a
//     client that closes the tab mid-pass cancels both auth checks,
//     shouldObserveCredentials bails on a check error, no live chat session is
//     told and the identity baseline never advances. The spellings are
//     `CheckNow(req.Context())`; `ctx := req.Context()` then `CheckNow(ctx)`;
//     and — the one that looks most like the real thing —
//     `context.WithTimeout(req.Context(), 45*time.Second)`, which is a detached
//     context in shape only: the deadline is not the only thing that can cancel
//     it. Catching all three takes THREE assertions, because each defeats a
//     different one: the argument must be an identifier (kills the first), the
//     defer must build its context from a ROOT (kills the second), and
//     `req.Context` must not appear inside the defer at all (kills the third,
//     which satisfies both of the others).
//   - drop the Flush: jsonResponse writes into net/http's bufio writer and does
//     not flush, and the handler does not return until the defer completes — so
//     the browser waits out the whole re-check on a request it has already been
//     answered for, and a fetch with a timeout aborts an import that succeeded.
//   - drop the `!result.Wrote` guard: a rejected paste spends a full in-process
//     re-check, two validate round-trips, on a file nobody touched.
func TestImportHandlerEndsInADetachedFlushedRecheck(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookies.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cookies.go: %v", err)
	}
	handler := routeHandlerLit(t, file, "/api/cookies/import")

	var deferred *ast.FuncLit
	ast.Inspect(handler, func(n ast.Node) bool {
		if deferred != nil {
			return false
		}
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if lit, ok := d.Call.Fun.(*ast.FuncLit); ok {
			ast.Inspect(lit, func(inner ast.Node) bool {
				if sel, ok := inner.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckNow" {
					deferred = lit
					return false
				}
				return true
			})
		}
		return true
	})
	if deferred == nil {
		t.Fatal("the import handler has no deferred CheckNow. A credential write that reaches no " +
			"refresh pass is invisible until the 30-minute ticker: the Twitch auth mark taken under " +
			"the old pair stands over a file that no longer has that problem, and no live chat " +
			"session is told to reconnect")
	}

	var callsFlush, guardsOnWrote, passesAnIdent, buildsOwnContext, derivesFromRequest bool
	ast.Inspect(deferred, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Flush" {
				callsFlush = true
			}
			// The context is built from a ROOT, and `context.WithTimeout` is
			// NOT one — deliberately, because it is the call that makes the
			// dangerous mutation look correct: `context.WithTimeout(req.Context(),
			// 45*time.Second)` is a timeout on a context that still cancels with
			// the tab, and accepting any WithTimeout would wave it straight
			// through. The parent is the whole question, so only the roots
			// themselves count.
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" &&
				(sel.Sel.Name == "Background" || sel.Sel.Name == "WithoutCancel") {
				buildsOwnContext = true
			}
			if sel.Sel.Name == "CheckNow" && len(v.Args) == 1 {
				// And the argument is an identifier rather than a call, so what
				// is handed over is the context built above rather than
				// req.Context() inline.
				_, isIdent := v.Args[0].(*ast.Ident)
				passesAnIdent = isIdent
			}
		case *ast.SelectorExpr:
			if v.Sel.Name == "Wrote" {
				guardsOnWrote = true
			}
			// req.Context ANYWHERE inside the defer, whatever it is wrapped in.
			// This is the assertion the other two cannot make: a WithTimeout
			// around the request context passes both of them, and only the
			// presence of this selector gives it away.
			if x, ok := v.X.(*ast.Ident); ok && x.Name == "req" && v.Sel.Name == "Context" {
				derivesFromRequest = true
			}
		}
		return true
	})

	if !guardsOnWrote {
		t.Error("the deferred re-check does not consult result.Wrote — a rejected paste spends two " +
			"validate round-trips on a file nobody touched")
	}
	if !callsFlush {
		t.Error("the deferred re-check does not Flush before running. jsonResponse writes into a " +
			"bufio writer that is not flushed and the handler does not return until this defer " +
			"completes, so the client waits out the whole re-check")
	}
	if !passesAnIdent {
		t.Error("CheckNow is called with an expression rather than a context identifier — if that is " +
			"req.Context(), a client that navigates away cancels the fingerprint comparison its own " +
			"import caused")
	}
	if !buildsOwnContext {
		t.Error("the deferred re-check never calls context.Background (or context.WithoutCancel), so " +
			"whatever identifier it hands CheckNow came from somewhere else. `ctx := req.Context(); " +
			"refreshSvc.CheckNow(ctx)` satisfies the identifier check above on its own, and is " +
			"exactly the mutation this assertion exists for. context.WithTimeout is deliberately not " +
			"accepted here: it says nothing about the parent, which is the whole question")
	}
	if derivesFromRequest {
		t.Error("the deferred re-check reaches req.Context() — a timeout wrapped around the REQUEST's " +
			"context is not a detached one: it still cancels when the client goes away, and " +
			"`context.WithTimeout(req.Context(), 45*time.Second)` satisfies both checks above while " +
			"doing exactly the thing they exist to prevent. Build from context.Background() instead. " +
			"(A deliberate context.WithoutCancel(req.Context()) — the request's VALUES without its " +
			"cancellation — would also trip this; that is a decision worth making explicitly here, " +
			"with a reason, rather than one that slips in.)")
	}
}
```

- [ ] **Step 8: Run both route tests to verify they fail**

Run: `go test -count=1 -run 'TestImport' ./internal/web/routes/`
Expected: FAIL to compile — `undefined: cookieImportOutcome`. Nothing in the package runs, so `routeHandlerLit`'s own fatal ("no POST handler is registered for /api/cookies/import") is not reported here; that is what you would see if the package compiled with the route still unregistered.

- [ ] **Step 9: Write the route**

In `internal/web/routes/cookies.go`, extend the import block:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)
```

Above `CookieRoutes` (`:242`), add the cap, the body reader and the renderer:

```go
// maxCookieImportBytes caps POST /api/cookies/import.
//
// A Netscape export of a signed-in YouTube + Twitch profile is a few kilobytes;
// 512 KiB is three orders of magnitude of headroom and still inside the
// chain-wide 1 MiB MaxBodySize (internal/web/server.go), so THIS is the limit
// that fires and its message is the one the operator reads. MaxBytesReader
// wrappers NEST rather than override — an inner SMALLER limit errors first,
// which is the direction that makes this cap real; the import endpoint's 500 MB
// reader is exempted from MaxBodySize for the opposite reason.
const maxCookieImportBytes = 512 << 10

// readCookieImportBody pulls the Netscape text out of either accepted request
// shape, answers the client itself on every refusal, and reports whether it
// did. Nothing is written when it returns false.
//
// TWO shapes because the panel has two controls, and they are the two a browser
// can send without ceremony: the textarea POSTs its text/plain contents, the
// file picker POSTs a multipart form with the file in a `cookies` part. Every
// other content type is refused rather than guessed at — a JSON body from a
// well-meaning client would otherwise be read as cookie text and rejected with
// a sentence about Netscape format, which describes the wrong mistake.
//
// The empty-body check is here rather than in prepareCookieImport because an
// empty request is a REQUEST-shape problem: "that cookie file has no cookie
// rows" is a claim about a file, and the operator sent none.
func readCookieImportBody(rw http.ResponseWriter, req *http.Request) (string, bool) {
	req.Body = http.MaxBytesReader(rw, req.Body, maxCookieImportBytes)

	var raw []byte
	var err error
	switch contentType := req.Header.Get("Content-Type"); {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		// Parsed explicitly so an oversize upload surfaces as the cap error
		// below rather than as "no cookies part" — FormFile would swallow the
		// MaxBytesError into a generic parse failure and answer the wrong
		// sentence.
		if perr := req.ParseMultipartForm(maxCookieImportBytes); perr != nil {
			err = perr
			break
		}
		file, _, ferr := req.FormFile("cookies")
		if ferr != nil {
			jsonError(rw, "the upload has no `cookies` file part", http.StatusBadRequest)
			return "", false
		}
		defer file.Close()
		raw, err = io.ReadAll(file)
	case contentType == "",
		strings.HasPrefix(contentType, "text/plain"),
		strings.HasPrefix(contentType, "application/octet-stream"):
		raw, err = io.ReadAll(req.Body)
	default:
		jsonError(rw, "send the cookie file as text/plain, or as a multipart upload in a `cookies` part",
			http.StatusUnsupportedMediaType)
		return "", false
	}

	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonError(rw, fmt.Sprintf("that cookie file is larger than the %d KiB this endpoint accepts",
				maxCookieImportBytes/1024), http.StatusRequestEntityTooLarge)
			return "", false
		}
		jsonError(rw, "could not read the request body", http.StatusBadRequest)
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		jsonError(rw, "no cookie data was supplied", http.StatusBadRequest)
		return "", false
	}
	return string(raw), true
}

// cookieImportOutcome renders one operator-supplied cookie import onto the
// wire.
//
// The KEY SET IS cookieSetupOutcome's, exactly, and that is the point rather
// than a coincidence: the two answer the same question about the same three
// states, the dashboard already has copy for it (cookieSetupAcceptedToast /
// cookieSetupRejectedMessage in web/public/modules/utils.js), and a fourth
// phrasing of "saved, but we could not check them" is how the copy drifts
// apart. See cookieSetupOutcome for what each key means and why `authenticated`
// and `*Verification` can disagree. Pinned by
// TestImportOutcomeSpeaksTheSetupOutcomeVocabulary.
//
// Deliberately NOT carrying a cookieStatus block. That comes from
// RefreshService.GetStatus, whose pass has not run yet at the moment this is
// rendered — the re-check is detached and fires after the response — so it
// would be the PRE-import snapshot: a status block contradicting the verdict
// beside it, which is precisely the junction defect the two payload projections
// above exist to prevent.
func cookieImportOutcome(result cookies.ImportResult) map[string]any {
	return map[string]any{
		"success":             true,
		"authenticated":       result.YouTubeAccepted,
		"twitchAuthenticated": result.TwitchAccepted,
		"youtubeVerification": result.YouTube.String(),
		"twitchVerification":  result.Twitch.String(),
	}
}
```

Immediately after the `/api/cookies/auto-refresh` handler (`:420`), the handler itself:

```go
	// POST /api/cookies/import — the browser-free re-authentication path.
	//
	// The transport half of a mechanism that was otherwise complete: replacing
	// the bytes of cookies.txt already reaches jar.Reload, the guide check,
	// OnAuthRecovered and the parked-job resume. What was missing is a way to
	// deliver those bytes without shell or filesystem access to the volume,
	// which is every container deployment and every remote dashboard.
	//
	// ANY AUTHENTICATED CLIENT, not loopback-gated, by owner ruling. The setup
	// wizard's loopback gate protects an UNCLAIMED instance from being claimed
	// by a stranger; this endpoint exists only behind an authenticated session
	// on an already-claimed one, and a loopback-gated ingest would be useless
	// in precisely the deployment it is built for — the operator reaches the
	// dashboard over the LAN or a tunnel, never from inside the container. The
	// capability it grants an attacker who already holds a session and a CSRF
	// token is to install cookies they already possess, which is strictly
	// smaller than the session they already have.
	//
	// There is NO GET, and there must never be one, however natural it feels
	// beside an upload control. That single asymmetry — accepts credential
	// bytes, never serves them — is what keeps this from being an exfiltration
	// path. Pinned by TestImportRouteHasNoGET.
	heavy.Post("/api/cookies/import", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		netscape, ok := readCookieImportBody(rw, req)
		if !ok {
			return
		}

		result, err := autoCookieSvc.ImportCookies(req.Context(), netscape)

		// The same deferred, detached, flushed re-check the setup-wizard finish
		// runs, and for the same reasons — see that handler for the full
		// derivation. In short: refresh's status block is the only place the
		// credential fingerprint is compared and the Twitch auth mark cleared;
		// a request-scoped context would let a closed tab cancel the comparison
		// its own import caused; and the Flush is what stops the client waiting
		// out a re-check it has already been answered for.
		//
		// Deferred rather than placed after the response so it covers the
		// jar-reload error exit too — the one error path that runs over a file
		// already replaced, and the one where a re-check is worth most, because
		// refresh's own jar.Reload repairs the stale in-memory jar.
		defer func() {
			if refreshSvc == nil || !result.Wrote {
				return
			}
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
			recheckCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			refreshSvc.CheckNow(recheckCtx)
		}()

		if err != nil {
			switch {
			// The three refusals, verbatim. Each names the operator's next move
			// and none of them quotes the submitted text; flattening them to a
			// 500 would answer a bad export with a server fault.
			case errors.Is(err, cookies.ErrImportNotNetscape),
				errors.Is(err, cookies.ErrImportNoRows),
				errors.Is(err, cookies.ErrImportNoCredential):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			// S9's abort: Moombox could not read the existing cookies.txt and
			// deliberately did not write. Verbatim, for the reason the sentinel's
			// doc gives — the fix is the permission or the mount, and this is the
			// one message that must never read as "replace your cookies", over a
			// file the endpoint just refused to touch.
			case errors.Is(err, cookies.ErrCookieFileUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			// A CONDITION the operator changes and then retries — the same shape
			// as the locked cookie DB and the blocked ladder, and 409 for the
			// same reason. The message names the single-file bind mount, which
			// is what actually produces it in a container.
			case errors.Is(err, cookies.ErrCookieFileUnwritable):
				jsonError(rw, err.Error(), http.StatusConflict)
			// The write LANDED and this process could not load it. Saying "the
			// import failed" would be false about the file on disk, and would
			// send an operator to repeat an import that already worked.
			case result.Wrote:
				jsonError(rw, "the cookies were imported and written, but this process could not load "+
					"them — the auth re-check that follows re-reads the file", http.StatusInternalServerError)
			default:
				jsonError(rw, "cookie import failed", http.StatusInternalServerError)
			}
			return
		}

		response := cookieImportOutcome(result)
		// ReloginStatus, not GetStatus: this reads nothing but the relogin map,
		// which ImportCookies has just updated, and GetStatus's browser/registry
		// detection scan would otherwise run for a field this never uses.
		response["autoCookieReloginRequired"] = autoCookieSvc.ReloginStatus()
		if getActivePlatforms != nil {
			response["activePlatforms"] = getActivePlatforms()
		}
		jsonResponse(rw, response)
	})
```

- [ ] **Step 10: Run the route tests to verify they pass**

Run: `go test -count=1 -run 'TestImport' ./internal/web/routes/`
Expected: PASS — 9 tests including subtests. Nine rather than eight: the filter also matches `TestImportHandlerEndsInADetachedFlushedRecheck` from Step 7.

- [ ] **Step 11: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: no `gofmt` output; 27 `ok` lines / 0 fail from ONE run.

- [ ] **Step 12: Commit**

```bash
git add internal/cookies/cookie_import.go internal/cookies/cookie_import_service_test.go \
        internal/cookies/autocookies.go internal/cookies/autocookies_profile.go \
        internal/web/routes/cookies.go internal/web/routes/cookies_import_test.go \
        internal/web/routes/cookies_import_callsite_test.go
git commit -F - <<'MSG'
feat(cookies,web): POST /api/cookies/import -- the browser-free re-auth path

Arc 11 Task 1. A container whose YouTube or Twitch session dies has no browser
and, until now, no way to supply fresh credentials except shell or filesystem
access to the volume. Everything downstream of "the bytes land in cookies.txt"
already worked; this is the transport.

ImportCookies is the fifth writer of cookies.txt and inherits Arc 2's catalogue
whole: the read goes through the readCookieFile seam and ABORTS on anything but
ENOENT rather than replacing a file it could not read, the merge is
mergeCookieFiles, the write is writeFileAtomic, and a failed write names the
single-file bind mount instead of answering a bare 500. The verdict comes from
the import's own checkPlatformAuth, so a signed-out export is reported at paste
time rather than at the next members-only stream, and the acceptance predicate
is now one function shared with the wizard rather than two copies of three
clauses.

The route is behind session auth and the CSRF middleware, on the heavy limiter,
capped at 512 KiB, and accepts either a text/plain paste or a multipart upload.
It ends in the setup-wizard finish's deferred, detached, flushed CheckNow, so
the write reaches the one place the credential fingerprint is compared and the
Twitch auth mark cleared instead of waiting on the 30-minute ticker. There is no
GET and there must never be one: accepting credential bytes and never serving
them is the whole of what keeps this from being an exfiltration path.

Every answer the endpoint can give is driven in a test with a body full of
distinctive fake values, and the response, its headers and every log line are
scanned for each of them -- the abort and write failures included, at the layer
where their seams live. Those three wrap an OS error and so carry a path, which
is intended and is not a secret; what the scans prove is that no cookie VALUE
rides along with it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---
### Task 2: `FlagManualRelogin`, restored with its caller

Spec R6. `AutoCookieService.FlagManualRelogin` was deleted in Arc 8 Task 12a (`c9cdf5c`) as exported, documented surface called by nothing but three tests — "it was written for the unbuilt re-auth ingest path, and dead exported surface on a security-sensitive service reads to the next reader as a wired feature". The ingest path is now built, so the setter comes back **in the same commit as the call site**, and the two test files that record the deletion carry the restore condition in their own comments.

**Which callers, and why TWO.** The re-login flag is what puts `YT: Re-login` / `TW: Re-login` in the dashboard header (`web/public/app.js:971-974`) and the `Relogin` badge in the TUI status bar. Today it has exactly one producer: `RefreshCookiesDetailed`'s verify-failed arm (`autocookies.go:2346-2351`), which requires a refresh pass that RAN and CHECKED. Two installs never get one, and they are two different installs:

| Install | What happens on a conclusive auth loss | Who raises the flag today |
|---|---|---|
| `auto_enabled = false` — **the documented container shape** (`docker/entrypoint.sh`, `SPEC.md`, `README.md`, `data-and-storage.md` all say leave it off) | `handleRecoveryNeeded`'s `!autoEnabled` branch (`monitor_callbacks.go:750-790`) logs, notifies and RETURNS. `runCookieRecovery` is never called at all. | nobody |
| `auto_enabled = true`, no browser and no profile | `runCookieRecovery` runs and `RefreshCookiesDetailed` returns `ErrNoBrowserFound` / `ErrProfileNotFound` before verifying anything | nobody |
| `auto_enabled = true` with a mounted profile | the browser-free import path RUNS and verifies, so the existing arm at `autocookies.go:2346` raises it | `RefreshCookiesDetailed` |

So the raise goes in BOTH exits of `handleRecoveryNeeded` — the disabled branch and, downstream of the enabled one, `runCookieRecovery`'s error branch. One site would leave the cohort R6 names, the container, with a dead session and no badge; and the header warning Task 3 routes to the import panel would never light there.

**No false prompt is possible at either exit**, and that is a property of what is upstream rather than of the two call sites: both sit behind `shouldFireRecovery` (`internal/cookies/refresh.go`), which fires only on `checkErr == nil && !nowAuth` — a conclusive not-authenticated. Wherever the raise lands, "a human has to sign in again" is TRUE about the credentials. (`livenessRecoveryArmed` is false, so the liveness probes cannot reach here at all; the note at `monitor_callbacks.go:761-775` is the derivation and it survives arming, because `ObserveLiveness` takes only conclusive verdicts.)

**An over-approximation on the enabled side, deliberately.** `runCookieRecovery`'s error branch also carries a locked cookie DB and other transient refresh failures, where the human's next move might be "close Firefox" rather than "sign in again". That is about the REMEDY, not about the credentials, which are conclusively dead in every case that reaches it. The flag is process-local and every accepted refresh, setup or import clears it, so a spurious raise costs one badge until the next pass; a missing raise costs a container operator any indication at all. Getting it wrong in the cheap direction is the trade this takes.

**`ErrCookieFileUnreadable` is excluded, and that exclusion is load-bearing.** There the credentials may be perfectly good and the file was simply not readable — nothing was written and nothing was checked. Raising "a human must sign in again" over a permissions or mount problem is the unearned cause this subsystem keeps finding, and it is the same reason that branch's notification refuses to say "replace cookies.txt". It is reachable only on the enabled path; the disabled branch runs no pass and so cannot produce it.

**Files:**
- Modify: `internal/cookies/autocookies.go` — restore `FlagManualRelogin` immediately above `StartSetup` (`:941`), i.e. straight after `refreshBrowser` ends at `:938`, which is where it sat before the deletion (`:830` is inside `ReloginStatus` today; find it by symbol, not by number)
- Modify: `cmd/moombox/monitor_callbacks.go:485-495` (`cookieReplacementGuidance` and its doc), `:750-790` (the `!autoEnabled` branch of `handleRecoveryNeeded` — the raise, and its stale "leads with the cookie FILE" comment), `:593-601` (the generic error branch), `:715-717` (the Ineffective notification's inline guidance)
- Test: `cmd/moombox/monitor_callbacks_relogin_test.go` (create); `internal/cookies/autocookies_relogin_status_test.go:130-155` (swap the direct-write fixture for the setter, rewrite the note); `internal/web/routes/cookies_relogin_status_test.go:27-50` (restore the value assertions the deletion removed, rewrite the note)
- Read (do not modify): `git show c9cdf5c^:internal/cookies/autocookies.go` for the deleted body; `cmd/moombox/monitor_callbacks_recovery_test.go:25-68` for `recoveryTestState` / `stubRefresh`

**Interfaces:**
- Consumes: `(*AutoCookieService).ReloginStatus() AutoCookieReloginRequired`; `runState.runCookieRecovery(ctx, platform string, refresh cookieRefresher, notify authFailureNotifier)`; `runState.handleRecoveryNeeded(platform string, autoEnabled bool, refresh cookieRefresher, notify authFailureNotifier)`; `recoveryTestState(t) (*runState, *[]sentNotification)`; `(*cookies.AutoCookieService).ImportCookies` (Task 1) for the clear side
- Produces: `(*cookies.AutoCookieService).FlagManualRelogin(platform string)`

- [ ] **Step 1: Write the failing tests**

Create `cmd/moombox/monitor_callbacks_relogin_test.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// withAutoCookieService gives a recovery test state a real AutoCookieService
// pointed at a temp cookie path, so the relogin map has a real owner.
func withAutoCookieService(t *testing.T, s *runState) *cookies.AutoCookieService {
	t.Helper()
	dir := t.TempDir()
	svc := cookies.NewAutoCookieService(dir, filepath.Join(dir, "cookies.txt"), cookies.NewCookieJar(), s.log)
	s.autoCookieSvc = svc
	return svc
}

// TestDisabledRecoveryRaisesTheReloginPrompt is the FIRST of the two exits, and
// the one that covers the documented container: `auto_enabled = false`.
//
// That branch never calls runCookieRecovery at all — it logs, notifies and
// returns (monitor_callbacks.go:750-790) — so every assertion in the sibling
// test below is silent about it, and the install every doc in the tree tells a
// container operator to run would show a dead session with no prompt anywhere.
//
// `refresh` is nil, safely: the disabled branch returns before it would be
// called, which is exactly the property that makes this exit a separate case.
//
// The mutation: deleting the raise from that branch. Every other test here stays
// green, because they all drive the enabled path.
func TestDisabledRecoveryRaisesTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	s.handleRecoveryNeeded("youtube", false, nil, recoveryNotifier(sent))

	relogin := svc.ReloginStatus()
	if !relogin["youtube"] {
		t.Error("automatic refresh is disabled, the session is conclusively dead, and no re-login " +
			"prompt was raised — this is the container's documented configuration, and the header " +
			"warning that routes to the import panel is the only thing that would tell the operator")
	}
	if relogin["twitch"] {
		t.Error("a YouTube auth loss raised the Twitch prompt — the flag is per platform")
	}
	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
	}
	if got := (*sent)[0].title; got != "Cookie Re-Authentication Required" {
		t.Errorf("notification title = %q, want the disabled branch's own", got)
	}
}

// TestRecoveryThatCouldNotRunRaisesTheReloginPrompt is the SECOND exit: the
// browser path is on, and there is nothing for it to run.
//
// RefreshCookiesDetailed returns ErrNoBrowserFound before it verifies anything,
// so the existing producer of the relogin flag — the verify-failed arm inside
// that function — cannot fire. (With a mounted profile it CAN: the browser-free
// import verifies, and autocookies.go:2346 raises the flag on its own. This
// exit is for the install that has neither.) Recovery itself only runs on a
// conclusive signed-out verdict, so reaching this branch means the credentials
// are dead AND nothing automatic can fix them.
//
// The mutation: deleting the FlagManualRelogin call from the generic error
// branch. The badge disappears for a `auto_enabled = true` host with no browser
// and no profile, which the disabled-branch test above cannot see.
func TestRecoveryThatCouldNotRunRaisesTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	noBrowser := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, cookies.ErrNoBrowserFound
	}
	s.runCookieRecovery(context.Background(), "youtube", noBrowser, recoveryNotifier(sent))

	relogin := svc.ReloginStatus()
	if !relogin["youtube"] {
		t.Error("a recovery that ran and could not act left no re-login prompt — with no browser and " +
			"no profile nothing else raises one, so the operator has no indication the session is dead")
	}
	if relogin["twitch"] {
		t.Error("the Twitch prompt was raised by a YouTube recovery — the flag is per platform and " +
			"this pass concluded nothing about Twitch")
	}
}

// TestUnreadableCookieFileDoesNotRaiseTheReloginPrompt.
//
// The abort means Moombox could not READ cookies.txt: nothing was written,
// nothing was checked, and the credentials in that file may be perfectly good.
// "A human must sign in again" is a claim about the credentials, and this
// branch has no basis for it — the same reason its notification refuses to say
// "replace cookies.txt".
//
// The mutation: hoisting the raise above the ErrCookieFileUnreadable arm, which
// is exactly where a careless "flag it on every error" would put it.
func TestUnreadableCookieFileDoesNotRaiseTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	unreadable := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, fmt.Errorf("%w — simulated", cookies.ErrCookieFileUnreadable)
	}
	s.runCookieRecovery(context.Background(), "youtube", unreadable, recoveryNotifier(sent))

	if svc.ReloginStatus()["youtube"] {
		t.Error("an unreadable cookies.txt raised the re-login prompt. Nothing was read, nothing was " +
			"written and nothing was checked — the credentials may be fine, and the remedy is the " +
			"permission or the mount")
	}
}

// TestSuccessfulRecoveryRaisesNoReloginPrompt is the other direction, and it is
// not padding: with only the positive case, raising the flag unconditionally at
// the top of runCookieRecovery passes.
func TestSuccessfulRecoveryRaisesNoReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshOK, cookies.RefreshOK), recoveryNotifier(sent))

	if svc.ReloginStatus()["youtube"] {
		t.Error("a recovery that RESTORED the session raised a re-login prompt")
	}
}

// TestRecoveryWithNoAutoCookieServiceDoesNotPanic. runState is assembled field
// by field and runCookieRecovery is driven from a nearly-zero one in four
// existing tests; a bare s.autoCookieSvc.FlagManualRelogin would panic there
// and take the monitor goroutine's recover with it.
//
// The mutation: dropping the nil guard.
func TestRecoveryWithNoAutoCookieServiceDoesNotPanic(t *testing.T) {
	s, sent := recoveryTestState(t) // no autoCookieSvc
	s.runCookieRecovery(context.Background(), "youtube",
		func(context.Context) (cookies.RefreshResult, error) {
			return cookies.RefreshResult{}, errors.New("simulated")
		}, recoveryNotifier(sent))
}

// TestAuthFailureGuidanceNamesTheDashboardImport.
//
// The guidance used to lead with the file on the volume because the only other
// remedy named was the interactive browser login, which drives a local headed
// browser and is unreachable from a container. The import is reachable from
// anywhere the dashboard is, so it leads now — and the operator most likely to
// be reading this notification is the one who cannot touch the volume.
//
// The mutation: reverting the copy. Asserted on the SENT notification rather
// than on the constant, because the constant is interpolated at four call sites
// and a %s dropped from one of them is its own defect.
func TestAuthFailureGuidanceNamesTheDashboardImport(t *testing.T) {
	s, sent := recoveryTestState(t)
	withAutoCookieService(t, s)

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshFailed, cookies.RefreshOK), recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want 1: %+v", len(*sent), *sent)
	}
	desc := (*sent)[0].desc
	for _, want := range []string{"Settings", "Cookies", "paste"} {
		if !strings.Contains(strings.ToLower(desc), strings.ToLower(want)) {
			t.Errorf("the auth-failure notification does not mention %q — a container operator is "+
				"told only about a file they cannot reach:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "%!s") || strings.Contains(desc, "%!(EXTRA") {
		t.Errorf("the guidance's format arguments no longer match its verbs: %s", desc)
	}
}
```

Add to `internal/cookies/cookie_import_service_test.go` (the clear side, now through the restored setter):

```go
// TestImportClearsAReloginFlagRaisedByTheSetter closes the loop R6 describes:
// the prompt path raises it, a successful import clears it. Task 1 asserted the
// clear against a direct write to the map because the setter did not exist yet;
// this drives the real pair.
//
// The mutation: FlagManualRelogin writing a platform key the clear does not
// read (or vice versa) — the prompt would then be unclearable by the one
// gesture that answers it.
func TestImportClearsAReloginFlagRaisedByTheSetter(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.FlagManualRelogin("youtube")
	if !s.ReloginStatus()["youtube"] {
		t.Fatal("FlagManualRelogin did not raise the YouTube flag")
	}

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if s.ReloginStatus()["youtube"] {
		t.Error("a successful import left the re-login prompt raised — the operator did the thing " +
			"the prompt asked for and is still being nagged about it")
	}
}

// TestFlagManualReloginTouchesOnlyTheNamedPlatform. The map is written under
// s.mu by three other paths and read by both UIs; a setter that wrote both keys
// would raise an alarm about a platform nothing concluded anything about, and
// one that wrote an arbitrary key would teach the frontend a platform it cannot
// render.
//
// The mutation: replacing the switch with `s.needsRelogin[platform] = true`.
func TestFlagManualReloginTouchesOnlyTheNamedPlatform(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.FlagManualRelogin("twitch")

	relogin := s.ReloginStatus()
	if !relogin["twitch"] {
		t.Error("the Twitch flag was not raised")
	}
	if relogin["youtube"] {
		t.Error("flagging Twitch raised YouTube too")
	}

	s.FlagManualRelogin("mastodon")
	if len(s.ReloginStatus()) != 2 {
		t.Errorf("an unrecognised platform was added to the map: %v — the wire shape is two keys and "+
			"the frontend iterates it", s.ReloginStatus())
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'Relogin|AuthFailureGuidance|NoAutoCookieService' ./cmd/moombox/ ./internal/cookies/`
Expected: FAIL to compile in both packages — `svc.FlagManualRelogin undefined`. Nothing runs, so the copy assertion in `TestAuthFailureGuidanceNamesTheDashboardImport` is not reported until Step 6; the widened pattern is what makes it, and `TestRecoveryWithNoAutoCookieServiceDoesNotPanic`, run at all — neither name contains "Relogin".

- [ ] **Step 3: Restore the setter**

In `internal/cookies/autocookies.go`, immediately above `StartSetup`:

```go
// FlagManualRelogin marks a platform as needing manual re-login.
//
// Exported because its callers are in cmd/moombox: handleRecoveryNeeded raises
// it at BOTH of its exits — the disabled branch, which is the container's
// documented configuration and never runs a pass at all, and the failed-recovery
// branch downstream of the enabled one. On either of those installs it is the
// only way the prompt is ever raised: RefreshCookiesDetailed's verify-failed
// arm, the other producer, needs a pass that got as far as checking, which
// wants either a browser or a mounted profile.
//
// It was deleted in Arc 8 Task 12a for having zero production callers, having
// been written for an ingest path that did not exist yet. It exists again
// because that path does (Arc 11), and it must not outlive that caller: an
// exported setter on a security-sensitive service with nothing calling it reads
// to the next reader as a wired feature.
//
// Process-local, like every other write to this map. Cleared per platform by
// FinishSetupDetailed, by RefreshCookiesDetailed's accepted arm and by
// ImportCookies — the gesture this flag is asking the operator to perform.
func (s *AutoCookieService) FlagManualRelogin(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A switch rather than a bare map write: the map's two keys are the wire
	// shape both UIs iterate, and an unrecognised platform must not widen it.
	switch platform {
	case "youtube":
		s.needsRelogin["youtube"] = true
	case "twitch":
		s.needsRelogin["twitch"] = true
	}
}
```

- [ ] **Step 4: Wire the caller and the copy**

In `cmd/moombox/monitor_callbacks.go`, replace `cookieReplacementGuidance` and its doc comment (`:484-493`):

```go
// cookieReplacementGuidance is the tail shared by the failure notifications.
//
// It leads with the DASHBOARD IMPORT on purpose, and the ordering has changed
// once: it used to lead with the cookie file on the volume, because the only
// other remedy it named was the interactive browser login, which opens a headed
// browser ON THE HOST and so is no use to a container (the image ships none) or
// to anyone reading the dashboard from another machine — exactly where this
// notification is most likely to be read. The
// import (Arc 11) is reachable from anywhere the dashboard is, needs no browser
// and no volume access, and is therefore the first thing to say. The file
// remains named because it still works and is the faster path for anyone with a
// shell. %s is the path to the configured cookie file.
const cookieReplacementGuidance = "Export a fresh Netscape cookies.txt from a browser signed in to the account, " +
	"then paste or upload it in Settings -> Cookies on the dashboard — no shell or volume access needed. " +
	"You can also overwrite the file at %s directly. (Export from a private window and close it: browsing on " +
	"in the source profile rotates the session and invalidates the export.) The interactive browser login in " +
	"Settings is an alternative only on the machine hosting Moombox."
```

In `handleRecoveryNeeded`'s `!autoEnabled` branch (`:750-790`), raise the flag immediately before its `notify` — and fix the comment above that `notify`, which is about to become false:

```go
		// Automatic refresh is off, so nothing here will attempt a repair, and
		// shouldFireRecovery has already concluded this platform is not
		// authenticated. That is exactly "a human has to sign in again", and on
		// this install — the container's documented shape — nothing else ever
		// says so: runCookieRecovery is not called on this path at all, and
		// RefreshCookiesDetailed's verify-failed arm needs a pass that got as far
		// as checking.
		if s.autoCookieSvc != nil {
			s.autoCookieSvc.FlagManualRelogin(platform)
		}
		// Guidance leads with the DASHBOARD IMPORT — see
		// cookieReplacementGuidance, whose ordering changed with Arc 11. This is
		// the notification most likely to be read somewhere the host's own
		// browser cannot be reached, and the import is the remedy that works
		// from there.
		notify(platform, "Cookie Re-Authentication Required",
			fmt.Sprintf("Moombox is not authenticated to %s, and automatic cookie refresh is turned off — nothing will "+
				"attempt to restore it on its own, so recordings that need an account will fail until the cookies are "+
				"replaced by hand. "+cookieReplacementGuidance, platform, s.cookieFilePath()),
			notifications.TypeError)
		return
```

The comment on the enabled branch below it says the same stale thing and gets the same fix:

```go
	// The pass itself, and the per-platform branch it takes, live in
	// runCookieRecovery — see cookieReplacementGuidance there for why the
	// notification copy leads with the dashboard import rather than the host's
	// own browser.
```

In the generic error branch (`:593-601`), raise the flag immediately before the notification. TWO sites, not one: this is the exit for `auto_enabled = true` with nothing to run, and the branch above is the exit for the flag being off — neither covers the other, and the container's documented configuration takes the other one.

```go
		s.log.Error("auto-cookie recovery failed", "platform", platform, "err", err)
		// The recovery was fired by a CONCLUSIVE signed-out verdict and the
		// automatic remedy for it failed, so a human has to sign in again. On an
		// install with the browser path ON but no browser and no profile, this is
		// the only line that ever says so: RefreshCookiesDetailed's own
		// verify-failed arm — the other producer of this flag — needs a pass that
		// got as far as checking, and there the pass returns ErrNoBrowserFound
		// before it verifies anything. (With a mounted profile it DOES get that
		// far and raises the flag itself; with the flag off, the disabled branch
		// above raises it.)
		//
		// Deliberately BELOW the ErrCookieFileUnreadable arm above, which
		// returns before reaching this. That abort read nothing, wrote nothing
		// and checked nothing; the credentials may be perfectly good, and
		// telling the operator to sign in again over a permissions or mount
		// problem is the unearned cause that arm's own message exists to avoid.
		//
		// An over-approximation for the rest — a locked cookie DB lands here too
		// — and deliberately so: the flag is process-local and the next
		// successful refresh, setup or import clears it, so a spurious raise
		// costs one badge, while a missing raise costs a container operator any
		// indication at all that the session is dead.
		if s.autoCookieSvc != nil {
			s.autoCookieSvc.FlagManualRelogin(platform)
		}
		// Previously log-only: the operator learned cookies were dead
		// only when a recording actually failed. 30-min per-platform
		// cooldown via notifyAuthFailure.
		notify(platform, "Cookie Auto-Refresh Failed",
			fmt.Sprintf("Automatic cookie refresh for %s failed — recordings will fail until the cookies are replaced. "+
				cookieReplacementGuidance, platform, s.cookieFilePath()),
			notifications.TypeError)
		return
```

In the `RefreshUnknown` arm's Ineffective notification (`:715-717`), replace the inline guidance tail so the two copies do not disagree about the remedy — old:

```
… so nothing has been concluded about the cookies (the log at debug level says which). If they have in fact expired, replace %s with a fresh Netscape export from a browser signed in to the account; the interactive browser login in Settings is an alternative only on the machine hosting Moombox.
```

new:

```
… so nothing has been concluded about the cookies (the log at debug level says which). If they have in fact expired, paste or upload a fresh Netscape export in Settings -> Cookies on the dashboard, or replace %s directly; the interactive browser login in Settings is an alternative only on the machine hosting Moombox.
```

- [ ] **Step 5: Update the two test files that carry the restore condition**

In `internal/cookies/autocookies_relogin_status_test.go` (`:136-143`), replace the direct write and its note:

```go
	// Through the setter again. It was a direct write for one arc, while
	// FlagManualRelogin was deleted as exported surface with zero production
	// callers; Arc 11 restored it WITH its caller (runCookieRecovery's failed
	// recovery), so the fixture can be the real writer once more.
	s.FlagManualRelogin("youtube")
```

In `internal/web/routes/cookies_relogin_status_test.go`, replace the block comment (`:27-50`) and put the value assertions back:

```go
// WHAT THESE TWO TESTS SAY, restored in Arc 11.
//
// They raise a platform's flag with AutoCookieService.FlagManualRelogin and
// assert the raised value comes back on the wire. That method was deleted in
// Arc 8 Task 12a — exported, documented and called by nothing but these tests,
// because the ingest path it was written for did not exist — and for one arc
// these tests could only say that the KEY was present and matched whatever the
// service reported, which a handler that had swapped the service call for the
// nil-service fallback literal would also satisfy on a fresh service.
//
// Arc 11 built the ingest path and restored the setter with its production
// caller, so both halves are pinned again: the key is present, and a RAISED
// value survives the handler unmangled.
```

and in each of the two tests, immediately after the service is constructed:

```go
	autoSvc.FlagManualRelogin("youtube")
```

with the existing `assertReloginMatchesService` call followed by:

```go
	if wire := reloginMapFromResponse(t, body); wire["youtube"] != true {
		t.Errorf("autoCookieReloginRequired[youtube] = %v, want the raised true — a handler that "+
			"answered with the nil-service fallback literal would look identical on a fresh service",
			wire["youtube"])
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 -run 'Relogin|AuthFailureGuidance|NoAutoCookieService' ./cmd/moombox/ ./internal/cookies/ ./internal/web/routes/`
Expected: PASS — six new `cmd/moombox` tests (both exits of `handleRecoveryNeeded`, the unreadable-file exclusion, the successful-recovery negative, the nil-service guard and the copy), two new `internal/cookies` tests, and the two restored routes tests.

- [ ] **Step 7: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: no `gofmt` output; 27 `ok` lines / 0 fail from ONE run.

- [ ] **Step 8: Commit**

```bash
git add internal/cookies/autocookies.go internal/cookies/autocookies_relogin_status_test.go \
        internal/cookies/cookie_import_service_test.go cmd/moombox/monitor_callbacks.go \
        cmd/moombox/monitor_callbacks_relogin_test.go internal/web/routes/cookies_relogin_status_test.go
git commit -F - <<'MSG'
feat(cookies,cmd): the re-login prompt reaches the cohort that cannot answer it

Arc 11 Task 2. FlagManualRelogin returns, with the caller it was written for.
Until now the prompt had exactly one producer -- RefreshCookiesDetailed's
verify-failed arm -- which needs a pass that got as far as checking. A container
has no browser to get there with: the pass returns ErrNoBrowserFound before it
verifies anything, so the one install with no automatic repair path was also the
one install that never saw the badge telling it to repair by hand.

handleRecoveryNeeded is where that is known, and the raise goes in BOTH of its
exits: the disabled branch, which is the container's documented configuration
and never calls runCookieRecovery at all, and the failed-recovery branch
downstream of the enabled one. One site alone would leave the cohort this arc
exists for -- auto_enabled = false, no browser -- with a dead session and no
badge. Neither exit can raise a false prompt: both sit behind
shouldFireRecovery's conclusive not-authenticated. The unreadable-cookie-file
abort is excluded and stays excluded -- it read nothing, wrote nothing and
checked nothing, and "sign in again" over a mount problem is the unearned cause
that arm's own message exists to avoid.

The guidance now leads with the dashboard import rather than the file on the
volume, because the operator most likely to be reading it is the one who cannot
reach the volume; the Ineffective notification's inline copy follows it so the
two do not disagree about the remedy. The two test files that recorded the
deletion carry their restore condition out: the routes tests raise a flag and
assert the raised value survives the handler again.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---

### Task 3: The settings panel and the re-login prompt's route to it

Spec R7. A paste textarea and a file picker in the cookies settings panel, the verdict rendered inline, and the re-login prompt pointing at it.

**What can honestly be tested, and how.** `settings.js` is not DOM-free, but it IS runnable: `settingsPanelVM` (`internal/web/routes/cookies_lasterror_panel_test.go:33-44`) loads the shipped module into goja with two transforms (strip the `utils.js` import, strip `export`), and `autoStatusPanelProbe` (`:55-90`) drives a real method against a stub `document` and a stub `fetch`, collecting what landed on every element in a SECOND `RunString` because goja drains the promise job queue when the enclosing one returns. **That harness is not enough here and the difference matters:** it strips the `utils.js` import and puts nothing back, which is invisible to its one existing consumer (`loadAutoCookieStatus` calls no helper from that module) and fatal to this one — `importCookies` calls three, and an unresolved identifier becomes a `ReferenceError` that the method's own `try/catch` swallows into the result div. So this task adds `settingsPanelVMWithUtils`, which evaluates the export-stripped `utils.js` into the same runtime first. Both new methods are testable that way, `FormData` included (a five-line recorder class is enough to assert the multipart branch built the right part). The pure decision the two click handlers share goes in `utils.js` and is run directly by `utilsVM` (`cookies_setup_utilsvm_test.go:48-58`), the way `cookieIndicatorState` is. Nothing here needs a source-shape assertion.

**The re-login prompt's target is decided by WHO IS ASKING, and only then by the host.** Clicking `YT: Re-login` today calls `startAutoCookieSetup(platform)` — the headed browser wizard, which opens a login window ON THE MACHINE RUNNING MOOMBOX. Two facts settle where the click should go, and the first outranks the second:

1. **The viewer must be at that machine.** A window that opens on the host is no remedy at all for someone holding a phone or a laptop on the LAN — they would click, see nothing, and have no way to know a browser is now waiting on a screen they cannot see. **Check this before assuming a gate already stops them: it does not.** `/api/setup/complete` — the FIRST-RUN wizard — is loopback-gated (`internal/web/routes/setup_routes.go:56-70`), and that is the gate the loopback doctrine refers to; the cookie setup trio in `internal/web/routes/cookies.go` is on `heavy` with no loopback gate at all, so a remote click really would launch a browser on someone else's screen today. The predicate is therefore the same shape as the server's (`location.hostname` being a loopback name mirrors `IsLoopbackRequest`, `internal/web/middleware.go`), but it is doing the work rather than echoing it.
2. **The host must actually have a browser.** `/api/cookies/auto-status` already answers that (`availableBrowsers`), and the container case is exactly its empty answer.

So: `"wizard"` only for a loopback viewer of an install that has a browser; `"import"` for everyone else — every remote or mobile viewer, every browserless host, and any install whose status could not be read. The fallback is safe in one direction only and that is the direction it takes: the panel `"import"` opens holds the Setup buttons too, so a local desktop operator who lands there loses one click, while a remote or container operator sent to the wizard loses the only route they have.

**The one case this rule gets wrong, named rather than implied.** An SSH local port-forward (`ssh -L 774:localhost:774 host`) shows the remote viewer `localhost` AND arrives at the server from loopback, so both signals agree and both are wrong: the click would open a browser on the HOST's screen, not on the viewer's. Nothing on either side can tell that apart — a forwarded connection is loopback by construction, which is the whole point of the forward — so no predicate fixes it and none is proposed. The operator who is tunnelling has the import box on the same panel, which works from there; the residual is that the wizard button next to it does not, exactly as it does not today.

**Files:**
- Modify: `web/public/index.html:1072-1083` (the import block, after the "Refresh cookies from browser profile" div and before the DPAPI divider)
- Modify: `web/public/modules/settings.js` — a listener in the constructor's wiring beside `btn-import-browser-profile` (`:392-416`), and `importCookies()` / `openCookieImport()` at the end of the Auto Cookie Methods section (after `cancelAutoCookieSetup`, `:2670`)
- Modify: `web/public/modules/utils.js` (append `reloginPromptTarget`)
- Modify: `web/public/app.js:475-495` (both re-login click handlers)
- Test: `internal/web/routes/cookies_import_panel_test.go` (create)

**Interfaces:**
- Consumes: `POST /api/cookies/import` and `cookieImportOutcome`'s key set (Task 1); `cookieSetupAcceptedToast(platformLabel, verification)`, `cookieSetupRejectedMessage(verification)`, `serverErrorMessage(response)` — all three already imported by `settings.js:4-12`; `settingsPanelVM`, `readEmbeddedModule`, `utilsVM`, `jsCall`
- Produces: `SettingsController.importCookies()`, `SettingsController.openCookieImport()`, `reloginPromptTarget(status, hostname)` returning `"wizard" | "import"`, and the element ids `cookie-import-text`, `cookie-import-file`, `btn-cookie-import`, `cookie-import-result`

- [ ] **Step 1: Write the failing tests**

Create `internal/web/routes/cookies_import_panel_test.go`:

```go
package routes

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// The import panel, asserted by RUNNING the shipped settings.js against a stub
// DOM — same technique, same reason, as cookies_lasterror_panel_test.go: three
// rounds of review found three defects where an assertion about JS was written
// as a string match and stayed green while the behaviour it named was broken.

// settingsPanelVMWithUtils is settingsPanelVM plus the module settingsPanelVM
// deliberately strips.
//
// settingsPanelVM (cookies_lasterror_panel_test.go:33) removes settings.js's
// `import {...} from "./utils.js"` line, because goja parses no ES modules, and
// nothing puts those helpers back. Its one existing consumer,
// loadAutoCookieStatus, references none of them — importCookies references
// three: serverErrorMessage, cookieSetupAcceptedToast and
// cookieSetupRejectedMessage. In a VM without them each is a ReferenceError
// thrown inside the method's own try/catch, which renders it into the result
// div: no toast is ever pushed, `failure` stays empty because the catch handled
// it, and three of the four tests below would fail against a perfectly correct
// implementation.
//
// utils.js is evaluated FIRST, under utilsVM's transform (strip `export`), so
// the helpers are ordinary global function declarations by the time settings.js
// is parsed. What the assertions then measure is the SHIPPED copy — the same
// cookieSetupAcceptedToast the browser calls, not a stub of it, which is what
// makes the hedged-copy assertion below worth anything.
func settingsPanelVMWithUtils(t *testing.T) *goja.Runtime {
	t.Helper()
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	utils = strings.ReplaceAll("\n"+utils, "\nexport ", "\n")

	settings := readEmbeddedModule(t, "public/modules/settings.js")
	settings = regexp.MustCompile(`(?s)import \{[^}]*\} from "\./utils\.js";`).ReplaceAllString(settings, "")
	settings = strings.ReplaceAll("\n"+settings, "\nexport ", "\n")

	vm := goja.New()
	if _, err := vm.RunString(utils); err != nil {
		t.Fatalf("utils.js does not evaluate — the browser would fail the same way: %v", err)
	}
	if _, err := vm.RunString(settings); err != nil {
		t.Fatalf("settings.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}
//
// FormData is stubbed as a recorder rather than skipped. The multipart branch is
// the one a phone uses, and "the file picker posts something" is the whole claim
// worth making about it from here; the server side of the same branch is pinned
// by TestImportRouteAcceptsAMultipartUpload.
const importPanelProbe = `
globalThis.__startImportPanel = function (opts) {
  const els = {};
  const mk = (id) => ({
    id, textContent: "", value: "", files: [], style: {},
    addEventListener() {}, focus() { this.focused = true; }, click() { this.clicked = true; },
  });
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = mk(id); return els[id]; },
    querySelector() { return { click() {} }; },
  };
  globalThis.setTimeout = function (fn) { fn(); return 0; };
  globalThis.FormData = function () { this.parts = {}; };
  globalThis.FormData.prototype.append = function (k, v) { this.parts[k] = v; };
  const sent = {};
  globalThis.fetch = function (url, init) {
    sent.url = url;
    sent.method = init && init.method;
    sent.contentType = init && init.headers && init.headers["Content-Type"];
    sent.bodyIsForm = !!(init && init.body instanceof globalThis.FormData);
    sent.body = init && init.body;
    return {
      ok: opts.ok,
      status: opts.status || (opts.ok ? 200 : 422),
      json() { return opts.body; },
      text() { return JSON.stringify(opts.body); },
    };
  };
  let failure = null;
  globalThis.console = { error(...a) { failure = String(a); } };

  const inst = Object.create(SettingsController.prototype);
  inst.app = { showToast(m, v) { (inst.__toasts = inst.__toasts || []).push({ message: m, variant: v }); },
               loadStatus() {}, escapeHtml(s) { return s; } };
  if (opts.text !== undefined) document.getElementById("cookie-import-text").value = opts.text;
  if (opts.file !== undefined) document.getElementById("cookie-import-file").files = [opts.file];
  inst.importCookies();

  globalThis.__collectImportPanel = function () {
    return {
      failure: failure === null ? "" : failure,
      result: els["cookie-import-result"] ? els["cookie-import-result"].textContent : "",
      color: els["cookie-import-result"] && els["cookie-import-result"].style.color || "",
      toasts: inst.__toasts || [],
      sent: sent,
    };
  };
};
`

type importPanelRun struct {
	failure string
	result  string
	color   string
	toasts  []map[string]any
	sent    map[string]any
}

func runImportPanel(t *testing.T, opts map[string]any) importPanelRun {
	t.Helper()
	vm := settingsPanelVMWithUtils(t)
	if _, err := vm.RunString(importPanelProbe); err != nil {
		t.Fatalf("install the import panel probe: %v", err)
	}
	if err := vm.Set("__opts", opts); err != nil {
		t.Fatalf("hand the probe its options: %v", err)
	}
	// Two RunStrings: the first starts the async handler and, on return, drains
	// the promise job queue that carries the rest of it.
	if _, err := vm.RunString("__startImportPanel(__opts);"); err != nil {
		t.Fatalf("importCookies threw — the browser would fail the same way: %v", err)
	}
	out, err := vm.RunString("__collectImportPanel();")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	raw, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T", out.Export())
	}
	run := importPanelRun{}
	run.failure, _ = raw["failure"].(string)
	run.result, _ = raw["result"].(string)
	run.color, _ = raw["color"].(string)
	run.sent, _ = raw["sent"].(map[string]any)
	if toasts, ok := raw["toasts"].([]any); ok {
		for _, tv := range toasts {
			if m, ok := tv.(map[string]any); ok {
				run.toasts = append(run.toasts, m)
			}
		}
	}
	if run.failure != "" {
		t.Fatalf("importCookies reported %q — the render did not complete", run.failure)
	}
	return run
}

// TestImportPanelPostsThePasteAndReportsTheVerdict.
//
// The mutations: posting to the wrong URL or with GET (the endpoint has no GET
// and answers 405, so the panel would report a transport error for every
// import); reading `data.success` instead of the per-platform fields (an
// import Moombox could not verify would be toasted as "cookies configured",
// which is the exact claim cookieSetupAcceptedToast exists to refuse).
func TestImportPanelPostsThePasteAndReportsTheVerdict(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "ok", "twitchVerification": "failed",
		},
	})

	if run.sent["url"] != "/api/cookies/import" {
		t.Errorf("posted to %v, want /api/cookies/import", run.sent["url"])
	}
	if run.sent["method"] != "POST" {
		t.Errorf("method = %v, want POST", run.sent["method"])
	}
	if len(run.toasts) != 1 {
		t.Fatalf("toasts = %v, want exactly one (YouTube accepted, Twitch not)", run.toasts)
	}
	if got, _ := run.toasts[0]["message"].(string); !strings.Contains(got, "YouTube") {
		t.Errorf("toast = %q, want the YouTube one", got)
	}
	if got, _ := run.toasts[0]["variant"].(string); got != "success" {
		t.Errorf("variant = %q, want success for a verified import", got)
	}
}

// TestImportPanelHedgesWhenTheCheckCouldNotConclude. Accepted is not verified:
// the operator's paste was saved and in use, and Moombox could not reach the
// site to confirm it.
//
// The mutation: dropping the verification argument from the toast call, which
// makes the helper's default arm report an unqualified success for a check that
// never concluded.
func TestImportPanelHedgesWhenTheCheckCouldNotConclude(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "unknown", "twitchVerification": "failed",
		},
	})
	if len(run.toasts) != 1 {
		t.Fatalf("toasts = %v, want one", run.toasts)
	}
	msg, _ := run.toasts[0]["message"].(string)
	if !strings.Contains(msg, "could not establish") {
		t.Errorf("toast = %q, want the hedged copy for an inconclusive check", msg)
	}
	if v, _ := run.toasts[0]["variant"].(string); v != "warning" {
		t.Errorf("variant = %q, want warning", v)
	}
}

// TestImportPanelShowsTheServersRefusalInline. The three refusals are the whole
// diagnostic value of the endpoint; a panel that renders "HTTP 422" throws it
// away.
//
// The mutation: `throw new Error("HTTP " + response.status)` instead of
// serverErrorMessage(response) — the shape the wizard's finish handler had
// before it was fixed.
func TestImportPanelShowsTheServersRefusalInline(t *testing.T) {
	const refusal = "that cookie file holds no YouTube or Twitch login cookie"
	run := runImportPanel(t, map[string]any{
		"ok": false, "status": 422,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{"error": refusal},
	})
	if !strings.Contains(run.result, refusal) {
		t.Errorf("inline result = %q, want the server's own sentence", run.result)
	}
	if run.color == "" {
		t.Error("the refusal is rendered in the panel's default colour and reads as help text")
	}
	if len(run.toasts) != 0 {
		t.Errorf("a refused import still toasted a success: %v", run.toasts)
	}
}

// TestImportPanelUploadsTheChosenFileAsMultipart. With the textarea empty and a
// file chosen, the request must carry the file in a `cookies` part — the part
// name the server reads.
//
// The mutations: posting the File object as a text/plain body (the server reads
// "[object File]" and answers "not a Netscape cookie file"); naming the part
// anything else (400, "the upload has no `cookies` file part").
func TestImportPanelUploadsTheChosenFileAsMultipart(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "",
		"file": map[string]any{"name": "cookies.txt"},
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "ok", "twitchVerification": "failed",
		},
	})
	if run.sent["bodyIsForm"] != true {
		t.Fatalf("the request body is not a FormData: %v", run.sent["body"])
	}
	if ct := run.sent["contentType"]; ct != nil && ct != "" {
		t.Errorf("Content-Type = %v — a multipart request must let the browser set its own "+
			"boundary; an explicit header makes the body unparseable", ct)
	}
	body, _ := run.sent["body"].(map[string]any)
	parts, _ := body["parts"].(map[string]any)
	if _, ok := parts["cookies"]; !ok {
		t.Errorf("the form has no `cookies` part: %v", parts)
	}
}

// TestReloginPromptTargetsTheImportUnlessTheWizardCanActuallyHelp is the pure
// decision the two header-warning handlers share, run out of the shipped
// utils.js.
//
// FOUR rows and none is padding, because there are two independent reasons the
// wizard is useless and each has its own mutant:
//
//   - drop the hostname test: a phone or a LAN laptop is sent to a wizard that
//     opens a login window on the HOST's screen. The click appears to do
//     nothing and nothing anywhere says why. The server does NOT refuse that
//     client — the loopback gate covers /api/setup/complete, not the cookie
//     setup trio — so this row is the only thing that stops it.
//   - drop the availableBrowsers test: a container operator sitting at the host
//     (docker exec, a local port-forward) is sent to a wizard that has no
//     browser to launch.
//   - invert either: a local desktop operator loses the one-click login for no
//     reason.
//   - the unreadable status is the fourth row: /auto-status can fail, and the
//     panel "import" opens holds BOTH controls, so answering "import" costs a
//     local user one click while answering "wizard" costs everyone else the only
//     route they have.
func TestReloginPromptTargetsTheImportUnlessTheWizardCanActuallyHelp(t *testing.T) {
	vm := utilsVM(t)
	withBrowser := map[string]any{"availableBrowsers": []any{map[string]any{"name": "Firefox"}}}
	noBrowser := map[string]any{"availableBrowsers": []any{}}

	for _, tc := range []struct {
		name     string
		status   any
		hostname string
		want     string
	}{
		{"at the host, with a browser", withBrowser, "localhost", "wizard"},
		{"at the host over IPv6 loopback, with a browser", withBrowser, "[::1]", "wizard"},
		{"at the host, no browser (the container shape)", noBrowser, "127.0.0.1", "import"},
		{"a LAN client of a host that has a browser", withBrowser, "192.168.1.20", "import"},
		{"a tunnelled client of a host that has a browser", withBrowser, "moombox.example.ts.net", "import"},
		{"the status could not be read, at the host", nil, "localhost", "import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := jsCall(t, vm, "reloginPromptTarget", tc.status, tc.hostname)
			if got != tc.want {
				t.Errorf("reloginPromptTarget(%v, %q) = %v, want %q", tc.status, tc.hostname, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestImportPanel|TestReloginPromptTarget' ./internal/web/routes/`
Expected: FAIL — `inst.importCookies is not a function`, and `utils.js no longer exports a callable reloginPromptTarget`.

- [ ] **Step 3: Add the markup**

In `web/public/index.html`, after the `auto-cookie-import-now` div closes (`:1082`) and before the `<sl-divider>` above the DPAPI switch (`:1083`). Match the surrounding indentation and the file's existing line endings — `.gitattributes` normalises `*.html` to LF in the index, so check `git diff --stat` afterwards shows only these lines and not the whole file:

```html
                            <sl-divider></sl-divider>
                            <h4>Paste or upload a cookies.txt</h4>
                            <p class="settings-help" style="margin-top: 0;">
                                Export cookies in Netscape format from a browser signed in to the
                                account, then paste the text or choose the file. Moombox MERGES what
                                you give it into the existing cookies.txt, so a YouTube-only export
                                leaves your Twitch session alone. This is the browser-free way to
                                re-authenticate: it needs no shell and no access to the data volume,
                                which makes it the path for Docker and for any dashboard you reach
                                over the network. Export from a private window and close it —
                                browsing on in the source profile rotates the session and
                                invalidates the export.
                            </p>
                            <sl-textarea
                                id="cookie-import-text"
                                rows="4"
                                resize="vertical"
                                placeholder="# Netscape HTTP Cookie File&#10;.youtube.com&#9;TRUE&#9;/&#9;TRUE&#9;…"
                                help-text="Paste the contents of cookies.txt. Leave empty to upload a file instead."
                            ></sl-textarea>
                            <input
                                type="file"
                                id="cookie-import-file"
                                accept=".txt,text/plain"
                                style="margin-top: 0.5em"
                            />
                            <div style="margin-top: 0.5em">
                                <sl-button id="btn-cookie-import" variant="primary" size="small">
                                    Import cookies
                                </sl-button>
                            </div>
                            <div
                                id="cookie-import-result"
                                style="margin-top: 0.4em; font-size: var(--sl-font-size-small)"
                            ></div>
```

- [ ] **Step 4: Add the decision to `utils.js`**

Append to `web/public/modules/utils.js`:

```js
/**
 * Where the header's "Re-login" warning should take the operator.
 *
 * Two remedies exist and only one of them works from everywhere. The
 * interactive browser login opens a REAL headed browser ON THE HOST; the import
 * needs no browser and works from wherever the dashboard is being read.
 *
 * TWO conditions, and the first outranks the second:
 *
 * 1. The viewer has to be AT the host, or a window opening there is no remedy —
 *    they would click, see nothing, and have no way to learn that a browser is
 *    waiting on a screen they cannot see. Nothing on the server stops them:
 *    /api/setup/complete (the FIRST-RUN wizard) is loopback-gated, but the
 *    cookie setup trio is not, so this predicate is the only thing between a
 *    remote click and a browser window on someone else's screen. It is the same
 *    shape as the server's IsLoopbackRequest deliberately, so the two read
 *    "local" the same way.
 * 2. The host has to HAVE a browser. /api/cookies/auto-status answers that, and
 *    the container case is its empty answer.
 *
 * Everything else goes to the import panel, including an unreadable status. The
 * asymmetry is deliberate: that panel holds BOTH controls, so a local operator
 * who lands there loses one click, while a remote or container operator sent to
 * the wizard loses the only route they have.
 *
 * @param {{availableBrowsers?: Array}|null|undefined} status - GET /api/cookies/auto-status
 * @param {string} hostname - location.hostname of the page making the request
 * @returns {"wizard"|"import"}
 */
export function reloginPromptTarget(status, hostname) {
  // A strict SUBSET of what the server's isLoopback accepts
  // (internal/web/middleware.go: net.ParseIP(ip).IsLoopback() plus the literal
  // "localhost", so all of 127.0.0.0/8 and every spelling of ::1). The four
  // below are the ones a browser actually puts in location.hostname; anything
  // else a local viewer might have typed — 127.0.0.2, 127.1, foo.localhost —
  // misses, and a miss errs toward "import", which costs that viewer one click
  // in a panel that holds both controls. Widen it only in that direction.
  const atTheHost = hostname === "localhost" || hostname === "127.0.0.1" ||
    hostname === "::1" || hostname === "[::1]";
  if (!atTheHost) return "import";
  const available = status && Array.isArray(status.availableBrowsers) ? status.availableBrowsers.length : 0;
  return available > 0 ? "wizard" : "import";
}
```

- [ ] **Step 5: Add the two methods to `settings.js`**

Wire the button beside the existing `btn-import-browser-profile` listener (`:392-416`):

```js
    const cookieImportBtn = document.getElementById("btn-cookie-import");
    if (cookieImportBtn) {
      cookieImportBtn.addEventListener("click", () => this.importCookies());
    }
```

and append the two methods after `cancelAutoCookieSetup` (`:2670`):

```js
  // POST /api/cookies/import — the browser-free re-authentication path.
  //
  // Two controls, one request. The textarea wins when it has content: a paste
  // is an explicit act, and a stale file left in the picker from a previous
  // attempt must not silently override what the operator just typed.
  //
  // The multipart branch sets NO Content-Type header, deliberately — the
  // browser must add its own multipart boundary, and an explicit header
  // overwrites it with one that has none, which makes the body unparseable at
  // the server for a reason nothing in the response can explain.
  async importCookies() {
    const resultEl = document.getElementById("cookie-import-result");
    const btn = document.getElementById("btn-cookie-import");
    const textEl = document.getElementById("cookie-import-text");
    const fileEl = document.getElementById("cookie-import-file");
    const pasted = (textEl && textEl.value || "").trim();
    const chosen = fileEl && fileEl.files && fileEl.files.length > 0 ? fileEl.files[0] : null;

    if (resultEl) {
      resultEl.textContent = "";
      resultEl.style.color = "";
    }
    if (!pasted && !chosen) {
      if (resultEl) {
        resultEl.textContent = "Paste the contents of a cookies.txt, or choose a file to upload.";
        resultEl.style.color = "var(--sl-color-warning-600)";
      }
      return;
    }
    if (btn) { btn.loading = true; btn.disabled = true; }

    try {
      let init;
      if (pasted) {
        init = { method: "POST", headers: { "Content-Type": "text/plain" }, body: pasted };
      } else {
        const form = new FormData();
        form.append("cookies", chosen);
        init = { method: "POST", body: form };
      }
      const response = await fetch("/api/cookies/import", init);
      // The endpoint's refusals are its whole diagnostic value — a signed-out
      // export, a JSON file, an unreadable cookies.txt, a single-file bind
      // mount. Rendering "HTTP 422" would throw every one of them away.
      if (!response.ok) throw new Error(await serverErrorMessage(response));
      const data = await response.json();

      const ytOk = data.authenticated;
      const twOk = data.twitchAuthenticated;
      if (ytOk || twOk) {
        // Accepted is not verified — the same three states the setup wizard
        // reports, rendered through the same helper, because a fourth phrasing
        // of "saved, but we could not check them" is how the copy drifts.
        if (ytOk) {
          const toast = cookieSetupAcceptedToast("YouTube", data.youtubeVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        if (twOk) {
          const toast = cookieSetupAcceptedToast("Twitch", data.twitchVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        // Clear the credentials out of the DOM the moment they are no longer
        // needed. They are already on the server; leaving them in a textarea
        // keeps a live session in the page for as long as it stays open.
        if (textEl) textEl.value = "";
        if (fileEl) fileEl.value = "";
        this.app.loadStatus();
      } else if (resultEl) {
        // Speaks from the verification fields, not from data.error: that never
        // exists on a 200.
        resultEl.textContent = cookieSetupRejectedMessage(
          ytOk === false && data.youtubeVerification !== "failed"
            ? data.youtubeVerification
            : data.twitchVerification,
        );
        resultEl.style.color = "var(--sl-color-danger-600)";
      }
    } catch (e) {
      if (resultEl) {
        resultEl.textContent = e.message;
        resultEl.style.color = "var(--sl-color-danger-600)";
      }
    } finally {
      if (btn) { btn.loading = false; btn.disabled = false; }
    }
  }

  // Open Settings on the cookies panel with the paste box focused. The route
  // the header's "Re-login" warning takes on an install where no browser can
  // run — see reloginPromptTarget.
  //
  // The tab click plus a deferred nav click is the pattern the empty-state CTA
  // already uses (web/public/app.js): the settings nav is an sl-menu and
  // nothing carries data-section, so the menu item is clicked by value once the
  // panel exists.
  openCookieImport() {
    document.querySelector('sl-tab[panel="settings"]')?.click();
    setTimeout(() => {
      document.querySelector('#settings-nav sl-menu-item[value="cookies"]')?.click();
      document.getElementById("cookie-import-text")?.focus();
    }, 100);
  }
```

- [ ] **Step 6: Route both re-login handlers through the decision**

In `web/public/app.js`, import `reloginPromptTarget` from `./modules/utils.js` beside the existing imports, and replace both handler bodies (`:475-495`):

```js
    // Status warnings — delegated click (text on desktop)
    const warningsEl = document.getElementById("status-warnings");
    if (warningsEl) {
      warningsEl.addEventListener("click", (e) => {
        const warning = e.target.closest(".status-warning");
        if (!warning) return;
        this.answerReloginPrompt(warning.dataset.action);
      });
    }

    // Status warnings — collapsed icon click (mobile)
    const warningsIconEl = document.getElementById("status-warnings-icon");
    if (warningsIconEl) {
      warningsIconEl.addEventListener("click", () => {
        this.answerReloginPrompt(warningsIconEl.dataset.action);
      });
    }
```

and add the shared handler as a method on the app class:

```js
  // Both re-login warnings land here. The remedy differs by WHO IS ASKING and
  // then by the install: the interactive browser login opens a window on the
  // host and its endpoints are loopback-gated, while the import works from
  // wherever the dashboard is being read. reloginPromptTarget decides; a status
  // that cannot be read falls back to the import panel, which holds both
  // controls.
  //
  // The auto-status fetch is on the CLICK rather than kept fresh in the
  // background: this is a rare, deliberate gesture, and one round trip on it
  // costs nothing while a cached answer could be a browser installed or removed
  // ago.
  async answerReloginPrompt(action) {
    if (action !== "yt-relogin" && action !== "tw-relogin") return;
    const platform = action === "tw-relogin" ? "twitch" : "youtube";
    let status = null;
    try {
      const response = await fetch("/api/cookies/auto-status");
      if (response.ok) status = await response.json();
    } catch {
      // Leave status null — reloginPromptTarget answers "import", which is the
      // safe direction.
    }
    if (reloginPromptTarget(status, window.location.hostname) === "wizard") {
      this.settings.startAutoCookieSetup(platform);
      return;
    }
    this.settings.openCookieImport();
  }
```

- [ ] **Step 7: Rebuild the embedded assets and run the tests**

```bash
go build ./...
go test -count=1 -run 'TestImportPanel|TestReloginPromptTarget' ./internal/web/routes/
```
Expected: PASS — 5 tests. The `go build` is not optional: the goja tests read `webassets.PublicFS`, so an un-rebuilt binary tests the previous copy of the module.

- [ ] **Step 8: Confirm the web assets are still LF**

```bash
git diff --stat -- web/public
```
Expected: only the four files, with small line counts. A file reported as wholly rewritten means its line endings flipped — restore it and re-apply the edit preserving the existing endings.

- [ ] **Step 9: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: no `gofmt` output; 27 `ok` lines / 0 fail from ONE run.

- [ ] **Step 10: Commit**

```bash
git add web/public/index.html web/public/app.js web/public/modules/settings.js \
        web/public/modules/utils.js internal/web/routes/cookies_import_panel_test.go
git commit -F - <<'MSG'
feat(web): a paste box and a file picker for cookies, and a re-login prompt that reaches them

Arc 11 Task 3. The cookies settings panel gains a textarea, a file picker and an
inline verdict, rendered through the same two copy helpers the setup wizard uses
so the three states have one vocabulary across both surfaces. The textarea wins
when it has content -- a paste is an explicit act and a stale file left in the
picker must not override it -- and the multipart branch sets no Content-Type,
because the browser has to add its own boundary. Credentials are cleared out of
the DOM as soon as the server has them.

The header's Re-login warning stops being a link to a browser that cannot run.
reloginPromptTarget reads the auto-status the panel already serves: where a
browser is available the click still opens the interactive login, and where none
is it opens the import panel instead. An unreadable status answers "import",
which is the cheap direction -- that panel holds both controls.

Every claim is made by RUNNING the shipped modules in goja against a stub DOM,
FormData included, rather than by matching source text.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---
### Task 4: The doc sentences that change with the code

Spec R8. Six documents assert, in the present tense, that this endpoint does not exist. Each edit below is an exact old → new pair; find the OLD text by searching for its first clause rather than by line number, because Tasks 0-3 changed no doc but this task's own earlier edits shift nothing else.

**Files:**
- Modify: `docs/spec/operations.md` (the UNBUILT paragraph, § Docker Image)
- Modify: `docs/spec/user-interfaces.md` (§ Cookies route table, the "shared API" sentence, the 503 sentence, the Web-UI render table)
- Modify: `docs/spec/data-and-storage.md` (§ Refresh Service's re-check caller list, § Auto-Cookie Service's opening, its Docker guidance)
- Modify: `SPEC.md` (§ Cookies: the acquisition-paths sentence and the "What is NOT built" sentence)
- Modify: `README.md` (the Docker cookie bullet, § Cookie Setup)
- Modify: `docker/entrypoint.sh` (the `[cookies]` comment block)
- Modify: `docs/superpowers/plans/2026-08-29-arc10-twitch-credential-lifecycle.md` (the reload-site table's last row)

**Interfaces:**
- Consumes: everything Tasks 0-3 built. No code changes in this task.
- Produces: nothing code depends on.

- [ ] **Step 1: `docs/spec/operations.md` — the UNBUILT paragraph flips**

OLD (§ Docker Image, the paragraph beginning "**The re-authentication ingest path is UNBUILT.**"):

> **The re-authentication ingest path is UNBUILT.** No endpoint accepts a pasted or uploaded cookie file: `CookieRoutes` (`internal/web/routes/cookies.go`) registers recheck, auto-refresh, the three auto-setup verbs plus abandon, auto-status and browser-path validation, and nothing else. So a container whose mounted profile has ALSO gone stale has no in-app way to supply fresh credentials — the file has to be replaced on the volume from outside. That is the gap the original containerised-cookies report named, and it is still open.

NEW:

> **Re-authentication without touching the volume.** `POST /api/cookies/import` accepts a Netscape cookie file — pasted as `text/plain` or uploaded as a multipart `cookies` part, capped at 512 KiB — behind session auth, the CSRF middleware and the shared `heavy` limiter. It MERGES rather than replaces (a YouTube-only paste leaves the Twitch rows alone), writes through `writeFileAtomic`, reloads the jar, verifies live, and answers with the same three-state per-platform verdict the setup wizard's finish returns, so a signed-out export is reported at paste time rather than at the next members-only stream. It is allowed from **any authenticated client**, not loopback-gated: the setup wizard's gate protects an unclaimed instance, and a loopback-gated ingest would be useless in exactly the deployment this exists for. **There is no corresponding GET and there must never be one** — accepting credential bytes and never serving them is what keeps it from being an exfiltration path. Replacing the file on the volume still works and is still the faster path for anyone with a shell.

- [ ] **Step 2: `docs/spec/user-interfaces.md` — the route table, the limiter sentence, the 503 sentence**

Add a row to the § Cookies table immediately after the `/api/cookies/auto-refresh` row:

```markdown
| `POST` | `/api/cookies/import` | shared API | Ingest an operator-supplied Netscape cookie file: `text/plain` body or multipart `cookies` part, 512 KiB cap. Merges into `cookies.txt`, reloads the jar, verifies, and returns `cookieSetupOutcome`'s exact key set. 400 empty/no part, 413 over the cap, 415 wrong content type, 422 for the three refusals and for an unreadable existing file, 409 for a failed write. **No GET.** |
```

OLD (the paragraph under the table):

> "shared API" is the single per-IP `apiRL` limiter (20 requests / 60s, `rateLimitAPIPerMinute`), shared with job creation and import. It wraps the endpoints that spawn or steer a browser: `AutoCookieService` already serialises them, but rate-limiting the request flow stops a caller burning CPU on the fast-fail path.

NEW (the "and import" clause was already stale — `ImportRoutes` builds its OWN 5/min limiter and the shared one is deliberately not applied there, `internal/web/routes/import_routes.go:60-65`):

> "shared API" is the single per-IP `apiRL` limiter (20 requests / 60s, `rateLimitAPIPerMinute`), shared with job creation. (The archive import at `/api/import` is NOT on it: it builds its own tighter 5/min limiter.) It wraps the endpoints that spawn or steer a browser — `AutoCookieService` already serialises them, but rate-limiting the request flow stops a caller burning CPU on the fast-fail path — and `POST /api/cookies/import`, which spawns nothing and is on it for a different reason: each request rewrites the credential file and makes up to four live auth round-trips.

OLD:

> `/auto-refresh` and the four `/auto-setup/*` endpoints answer `503 auto-cookie service not configured` when no `AutoCookieService` is wired.

NEW:

> `/auto-refresh`, `/import` and the four `/auto-setup/*` endpoints answer `503 auto-cookie service not configured` when no `AutoCookieService` is wired.

In the "What the Web UI renders" table, OLD:

> | Header warnings (`YT: Re-login` / `TW: Re-login`) | `autoCookieReloginRequired` | Text on desktop, a collapsed `exclamation-triangle` icon on mobile; clicking either starts interactive setup for that platform |

NEW (two rows):

> | Header warnings (`YT: Re-login` / `TW: Re-login`) | `autoCookieReloginRequired`, then `/api/cookies/auto-status` on the click | Text on desktop, a collapsed `exclamation-triangle` icon on mobile. Clicking asks `reloginPromptTarget` (`web/public/modules/utils.js`) which remedy is actually available to THIS viewer: interactive setup only for a loopback viewer of a host that has a browser, because the wizard opens a login window ON THE HOST and nothing server-side stops a remote client from triggering that (the loopback gate covers `/api/setup/complete`, not the cookie setup trio) — and the cookies panel's import box for everyone else, including every LAN or tunnelled client, every browserless host, and any install whose status could not be read (that panel holds both controls, so the fallback costs a local operator one click). **Known limit:** an SSH local port-forward reads as loopback on BOTH sides, so a viewer tunnelling in is offered the wizard and the window opens on the host; nothing client- or server-side can distinguish that, and the import box beside it is the working path |
> | Settings → paste/upload cookies (`cookie-import-text`, `cookie-import-file`, `btn-cookie-import`) | `POST /api/cookies/import` | The textarea wins when it has content; otherwise the chosen file goes up as multipart with no explicit `Content-Type` so the browser sets its own boundary. Accepted platforms toast through `cookieSetupAcceptedToast`, a rejection renders inline through `cookieSetupRejectedMessage`, and a non-200 renders the server's own sentence through `serverErrorMessage`. Both controls are cleared on success |

- [ ] **Step 3: `docs/spec/data-and-storage.md` — the writer list and the re-check caller list**

OLD (§ Auto-Cookie Service, first line):

> `AutoCookieService` acquires credentials into `cookies.txt` — through an interactive browser login, through a headless browser refresh, or browser-free by importing a browser profile directly.

NEW:

> `AutoCookieService` acquires credentials into `cookies.txt` four ways — an interactive browser login, a headless browser refresh, a browser-free import of a mounted browser profile, and `ImportCookies`, the operator-supplied Netscape file that `POST /api/cookies/import` delivers. That last one is the FIFTH writer of `cookies.txt` (with the refresh service's rotation write) and inherits the same catalogue as the rest: read through the `readCookieFile` seam and abort on anything but ENOENT, merge through `mergeCookieFiles` keyed by name+domain, write through `writeFileAtomic`, and never emit an empty-valued row — it filters those out of the paste before merging AND out of the merged text before writing, which also repairs any an older writer left behind.

OLD (§ Refresh Service, inside the single-flight paragraph):

> The two `internal/web/routes` callers — the dashboard/Settings browser refresh and the setup wizard's finish — call `CheckNow` bare, because that package has no operational logger; the Web wizard finish's copy is additionally DETACHED onto its own 45-second timeout rather than the request's context, so a client that navigates away cannot cancel the fingerprint comparison its own finish caused.

NEW:

> The three `internal/web/routes` callers — the dashboard/Settings browser refresh, the setup wizard's finish and `POST /api/cookies/import` — call `CheckNow` bare, because that package has no operational logger. The wizard finish and the import are additionally DETACHED onto their own 45-second timeout rather than the request's context, and both flush the response first, so a client that navigates away can neither cancel the fingerprint comparison its own write caused nor be made to wait out a re-check it has already been answered for. Each is gated on the pass having written (`SetupResult.Wrote` / `ImportResult.Wrote`), which is true on the jar-reload error exit as well as on success — the one error path that runs over a file already replaced, and the one where the re-check is worth most.

OLD (§ Auto-Cookie Service, the **Docker guidance** paragraph, first sentence):

> **Docker guidance.** Leave `auto_enabled` off, update the mounted browser profile, then press `R F` (or shift+click the header button, or the Settings-page twin).

NEW:

> **Docker guidance.** Two browser-free paths, and they answer different questions. To REFRESH from a mounted profile: leave `auto_enabled` off, update the profile on the host, then press `R F` (or shift+click the header button, or the Settings-page twin). To RE-AUTHENTICATE when the profile itself is stale or there is none: paste or upload a fresh Netscape export in Settings → Cookies, which needs no volume access at all and is the only path that works from a phone against a tunnelled instance.

- [ ] **Step 4: `SPEC.md` § Cookies**

OLD:

> **Auto-cookie service (`AutoCookieService`).** Acquires credentials three ways: an interactive browser login, a headless browser refresh (Firefox reads `cookies.sqlite` **together with its `-wal` sidecar**; Chromium is driven over CDP, with an opt-in Windows DPAPI read of the user's real profile as a fallback, off by default), or browser-free by importing a mounted browser profile.

NEW:

> **Auto-cookie service (`AutoCookieService`).** Acquires credentials four ways: an interactive browser login, a headless browser refresh (Firefox reads `cookies.sqlite` **together with its `-wal` sidecar**; Chromium is driven over CDP, with an opt-in Windows DPAPI read of the user's real profile as a fallback, off by default), browser-free by importing a mounted browser profile, or from an operator-supplied Netscape file posted to `POST /api/cookies/import`.

OLD:

> **Docker works, with one manual step.** Leave `auto_enabled` off (the image ships no browser), mount a Firefox profile, and press `R F` / shift+click / the Settings button after each host-side profile refresh. The very first import is automatic — but only when there is no `cookies.txt` to lose. What is NOT built is a re-authentication ingest path: no endpoint accepts a pasted or uploaded cookie file, so a container whose mounted profile has also gone stale must have the file replaced on the volume from outside.

NEW:

> **Docker works, with one manual step.** Leave `auto_enabled` off (the image ships no browser), mount a Firefox profile, and press `R F` / shift+click / the Settings button after each host-side profile refresh. The very first import is automatic — but only when there is no `cookies.txt` to lose. When the profile itself goes stale, `POST /api/cookies/import` takes a pasted or uploaded Netscape file from any authenticated client, merges it into `cookies.txt`, reloads the jar and answers with a live verdict — no browser, no shell, no volume access. It has no GET, deliberately and permanently.

- [ ] **Step 5: `README.md`**

OLD (the Docker cookie bullet, the sentence beginning "When the session does die"):

> When the session does die, the fix is to overwrite
>   `./data/cookies.txt` with a fresh export — the interactive browser login
>   in Settings needs a headed browser and a person at it, so it is not an
>   option here.

NEW:

> When the session does die, open Settings → Cookies
>   in the dashboard and paste (or upload) a fresh Netscape export: Moombox
>   merges it into `cookies.txt`, reloads it immediately and tells you whether
>   it authenticates. Overwriting `./data/cookies.txt` on the host works too and
>   takes effect within 30 minutes, or right away if you press "Refresh
>   cookies". The interactive browser login in Settings needs a headed browser
>   and a person at it, so it is not an option here.

Add a subsection to § Cookie Setup, between "Browser profile import (headless hosts and Docker)" and "### Manual":

```markdown
### Paste or upload (any host, no browser, no volume access)

The dashboard's Settings → Cookies panel takes a Netscape `cookies.txt`
directly: paste the text, or choose the file. Moombox **merges** it into
whatever `cookies.txt` already holds — a YouTube-only export leaves your Twitch
session alone, and vice versa — reloads it into the running process, and answers
with what a live check concluded, so a bad export is reported while you are
still looking at it rather than at the next members-only stream.

This is the re-authentication path for a container, and for any instance you
reach over the network or from a phone: it needs no browser on the host and no
access to the data volume. Jobs parked in `COOKIES?` resume on their own once
the credentials check out.

Export from a **private window** and close it afterwards: continuing to browse
in the source profile rotates the session and invalidates the export. Moombox
never serves cookies back — there is no download, by design.
```

- [ ] **Step 6: `docker/entrypoint.sh`**

In the generated `[cookies]` comment block, after the paragraph ending "…On a phone or tablet use the Settings button: shift+click needs a keyboard." and before "# The in-process refresh that keeps the imported YouTube session alive":

```sh
#
# NO PROFILE, OR THE PROFILE ITSELF IS STALE? Paste it instead. The
# dashboard's Settings -> Cookies panel accepts a Netscape cookies.txt
# by paste or upload: it MERGES into the file above (a YouTube-only
# export leaves Twitch alone), reloads it immediately and reports
# whether it authenticates. No browser, no shell, no access to the
# data volume -- which makes it the re-auth path for this image, and
# the only one that works from a phone. Export from a private window
# and close it: browsing on in the source profile invalidates it.
```

- [ ] **Step 7: The Arc 10 reload-site table**

In `docs/superpowers/plans/2026-08-29-arc10-twitch-credential-lifecycle.md`, the last row of the reload-site table. OLD:

> | Arc 11's import endpoint | not built | — | Arc 11 must end in `recheckAfterCookieWrite`; recorded in the Arc 11 handoff. |

NEW (and note what it corrects: `recheckAfterCookieWrite` lives in `cmd/moombox` and is unreachable from `internal/web/routes`, so the import took the routes-package shape the Web wizard finish already uses):

> | `POST /api/cookies/import` (Arc 11) | `internal/web/routes/cookies.go` — deferred `refreshSvc.CheckNow(recheckCtx)`, gated on `ImportResult.Wrote`, flushed first, on a detached 45 s context | yes | `TestImportHandlerEndsInADetachedFlushedRecheck` (AST, `cookies_import_callsite_test.go`). NOT `recheckAfterCookieWrite`, as this row previously anticipated: that helper lives in `cmd/moombox` and this package cannot see it. The import took the Web wizard finish's shape instead — same junction, same detach, same flush, no logger. |

- [ ] **Step 8: Verify every claim the docs now make**

```bash
grep -rn "UNBUILT" docs/ SPEC.md README.md
grep -rn "acquires credentials" docs/spec/data-and-storage.md
grep -rn "api/cookies/import" docs/ SPEC.md README.md docker/
```
Expected: the first prints nothing under `docs/spec/`; the second shows "four ways"; the third shows the route named in `operations.md`, `user-interfaces.md`, `data-and-storage.md`, `SPEC.md`, and the Arc 10 plan (README and `entrypoint.sh` describe the panel rather than the path, deliberately — an operator reads the dashboard, not the route table).

- [ ] **Step 9: Run the gates**

```bash
go build ./... && go vet ./... && GOOS=linux go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: unchanged from Task 3 — 27 `ok` lines / 0 fail. (`docker/entrypoint.sh` is not compiled; check it with `sh -n docker/entrypoint.sh`, which must print nothing.)

- [ ] **Step 10: Commit**

```bash
git add docs/spec/operations.md docs/spec/user-interfaces.md docs/spec/data-and-storage.md \
        SPEC.md README.md docker/entrypoint.sh \
        docs/superpowers/plans/2026-08-29-arc10-twitch-credential-lifecycle.md
git commit -F - <<'MSG'
docs: the re-auth ingest path is built, so six documents stop saying it is not

Arc 11 Task 4. operations.md's "The re-authentication ingest path is UNBUILT"
paragraph and SPEC.md's "What is NOT built" sentence both described the endpoint
this arc built, in the present tense. user-interfaces.md gains the route, its
status codes and the two Web surfaces that drive it; data-and-storage.md names
the fifth writer of cookies.txt and the third routes-package caller of CheckNow;
README and the entrypoint's generated config say plainly that re-auth in a
container is a paste, not a shell.

Two corrections ride along. The shared-limiter sentence claimed /api/import is
on apiRL; it is not -- ImportRoutes builds its own tighter 5/min limiter and the
shared one is deliberately withheld. And the Arc 10 reload-site table's last row
anticipated that Arc 11 would end in recheckAfterCookieWrite; that helper lives
in cmd/moombox and internal/web/routes cannot see it, so the import took the Web
wizard finish's shape -- same junction, same detach, same flush.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---

### Task 5: The live-gate decision, and the field gate that replaces it

**Decision: no new `MOOMBOX_LIVE_*` gate. Recorded here so it is a decision rather than an omission.**

A live gate earns its place when a mechanism can only be checked against the real upstream — a client that returns a well-formed 200 with no usable formats, a browser drain that cannot be faked, a token whose shape nothing in-tree has ever decoded. This endpoint introduces no new conversation with either upstream. Its verdict comes from `checkPlatformAuth`, which calls `VerifyYouTubeAuth` / `VerifyTwitchAuth` — the exact callbacks the setup wizard's finish and every refresh pass already use, wired in `cmd/moombox` to `RefreshService`'s guide POST and `oauth2/validate` GET, both of which already have live coverage (`internal/twitch/auth_live_test.go`, `internal/youtube/liveness_markers_live_test.go`, and `internal/cookies/autocookies_refresh_live_test.go` for the write path around them). A gate here would re-measure those through one more layer of plumbing.

What is genuinely unmeasured is not a Go assertion at all: whether a real operator's real export, pasted through a real browser into a real container, lands and works — including the two things no unit test can reach, the single-file bind-mount failure and a `multipart/form-data` body assembled by an actual browser rather than by `mime/multipart`. That is a field gate, and the tree has a table for those.

**Files:**
- Modify: `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md` (Part 4, "Every open field gate" — append row 15)

**Interfaces:**
- Consumes: the endpoint and the panel from Tasks 1 and 3.
- Produces: nothing code depends on.

- [ ] **Step 1: Append the field gate**

After row 14 ("First real Docker deployment"):

```markdown
| 15 | Arc 11 re-auth ingest (Arc 11 Task 5 decision: no live Go gate is warranted; this is what replaces it) | That a real export, pasted or uploaded through a real browser, lands on disk, reloads, verifies and resumes parked jobs — including the two things no unit test reaches: a browser-assembled multipart body, and the single-file bind-mount write failure | Any instance with real cookies. (a) Export a fresh Netscape cookies.txt from a signed-in private window on any desktop; paste it into Settings -> Cookies and press Import. (b) Repeat with the file picker instead of the textarea. (c) On a throwaway container ONLY, bind-mount cookies.txt as an individual file (`- ./cookies.txt:/data/cookies.txt`) and import again | (a) and (b): a green toast per accepted platform, the file on the volume gaining the pasted rows while keeping the sibling platform's, the header badge going green without a restart, and any `COOKIES?` job resuming. (c) a 409 whose message names the bind mount, and cookies.txt unchanged | A toast that says "configured" while the badge stays red (the verdict is being read from a pre-import snapshot); a merged file that lost the sibling platform's rows; a 500 with "cookie import failed" for the bind-mount case; or any cookie value visible in the response, the log or the notification |
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md
git commit -F - <<'MSG'
docs(plans): the ingest endpoint gets a field gate, not a live Go gate

Arc 11 Task 5. The import introduces no new conversation with either upstream --
its verdict comes from the same VerifyYouTubeAuth / VerifyTwitchAuth callbacks
the wizard finish and every refresh pass use, and those already have live gates
in internal/twitch, internal/youtube and internal/cookies. A MOOMBOX_LIVE_ gate
here would re-measure them through one more layer of plumbing.

What is actually unmeasured is a real export pasted through a real browser into
a real container: a browser-assembled multipart body and the single-file
bind-mount write failure are the two things no unit test reaches. Field gate 15.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
MSG
```

---

## Self-review

Run against the spec with fresh eyes after the plan was complete.

**1. Spec coverage.**

| Spec requirement | Task | Where |
|---|---|---|
| R1 — one endpoint, paste and/or multipart, size-capped, auth + CSRF + `heavy`, any authenticated client | 1 | `readCookieImportBody` (two shapes, 512 KiB), `heavy.Post`, `TestImportRouteIsOnTheHeavyLimiter`, `TestImportRouteRefusesTheWrongRequestShapes`. Session auth is chain-level (`cmd/moombox/services.go:1132` — `s.r.Use(webServer.AuthMiddleware)`) and CSRF is chain-level (`internal/web/server.go:112`); both apply to every `/api/` route by construction, the handler adds no exemption, and there is no per-route test because there is no per-route code to break. `AuthMiddleware` waives auth for loopback and private clients and for password-less installs (`internal/web/server.go:143-158`), so "any authenticated client" means "any client this chain already admits" — exactly the exposure of every other mutating route, which is the comparison the ruling makes, and not the wizard's separate loopback gate. |
| R2 — validate before touching disk, three refusals, value-free | 0 | `prepareCookieImport`'s switch + `netscapeCookiesHoldACredential`; `TestPrepareCookieImportRefusesTheThreeShapes`, `TestPrepareCookieImportRefusalsNameNoValue`, `TestImportRouteRefusesTheThreeShapesWithTheirOwnMessage` (which also asserts the file is untouched). |
| R3 — `readCookieFile` seam, `ErrCookieFileUnreadable` abort, merge not replace, `writeFileAtomic`, never an empty row, then reload | 0, 1 | `ImportCookies`; `TestImportCookiesAbortsOnAnUnreadableExistingFile`, `TestPrepareCookieImportMergesRatherThanReplaces`, `TestPrepareCookieImportNeverWritesAnEmptyValuedRow`, `TestImportCookiesMergesOntoDiskAndReloadsTheJar`. |
| R4 — the write ends in `CheckNow`, detached + flushed; the three-state verdict on the wire | 1 | The handler's deferred re-check; `TestImportHandlerEndsInADetachedFlushedRecheck`, `TestImportOutcomeSpeaksTheSetupOutcomeVocabulary`, `TestImportRouteWritesAndAnswersWithTheVerdict`. |
| R5 — no GET, ever; no download affordance | 1, 3 | Only `heavy.Post` is registered; `TestImportRouteHasNoGET`. The panel markup in Task 3 has no download control and the README says so. |
| R6 — `FlagManualRelogin` returns with its caller; the prompt names the import; a successful import clears it | 2 | Raised at BOTH exits of `handleRecoveryNeeded`, which is what makes it reach the cohort R6 names: `TestDisabledRecoveryRaisesTheReloginPrompt` (`auto_enabled = false`, the container's documented shape, where `runCookieRecovery` is never called) and `TestRecoveryThatCouldNotRunRaisesTheReloginPrompt` (flag on, nothing to run). Excluded where it would be unearned: `TestUnreadableCookieFileDoesNotRaiseTheReloginPrompt`. Not raised on success: `TestSuccessfulRecoveryRaisesNoReloginPrompt`. Copy: `TestAuthFailureGuidanceNamesTheDashboardImport`. Cleared: `TestImportClearsAReloginFlagRaisedByTheSetter`. |
| R7 — textarea + file picker + inline verdict; the re-login prompt links to it; TUI parity optional | 3, 4 | The panel and `reloginPromptTarget`; five goja tests. **TUI parity is SKIPPED**, and Task 4's `user-interfaces.md` edit says so by naming only the two Web surfaces — the TUI has no text-entry affordance for a multi-kilobyte paste and a terminal paste of a credential file into a bubbletea textarea is a worse experience than the file copy the TUI user already has. |
| R8 — README, container guidance, `operations.md`, `user-interfaces.md`, `data-and-storage.md`, `SPEC.md` | 4 | Exact old → new pairs, plus the Arc 10 reload-site row and two stale claims corrected. |
| §4 invariants — atomic write, merge semantics, empty-row trap, name+domain keying, the unreadable-file error and what renders it, the bind-mount trap, no value anywhere, inline recover, anonymous logger | 0, 1 | All covered above except: the bind-mount trap is `ErrCookieFileUnwritable` + `TestImportCookiesNamesTheBindMountOnAFailedWrite`; "no value anywhere" is `TestImportRouteLeaksNoValueAnywhere` (four wire answers, over a recording logger) plus `TestImportFailurePathsCarryNoValue` (the three seam-only failure paths, over the error, `lastError` and the log); no goroutine is added, so the recover rule is satisfied vacuously and the plan says so in Global Constraints; the logger stays the anonymous interface (`recordingLogger` implements the four methods without naming one). |
| §5 tests — every listed case | 0, 1, 2 | All present. The one the spec words as "the write is followed by exactly one `CheckNow` (the Arc 10 gate-shape test extends to this site)" is met DIFFERENTLY and deliberately: Arc 10's `TestNotePassCompletedIsGatedOnRanAtEverySite` pins call sites of `notePassCompleted` INSIDE `internal/cookies`, and this writer's caller is outside that package — extending it here would be wrong (it would demand a seam whose doc says it exists for exactly two in-package callers and that firing it elsewhere doubles every external site). The routes-package equivalent is `TestImportHandlerEndsInADetachedFlushedRecheck`, which asserts the same four properties by AST. Named as a deviation, not a gap. |

**Residual, named rather than hidden (leak scans):** the route's scan drives four answers — accepted, signed-out, not-Netscape and the unreadable-file abort; the unwritable-file and reload-failure answers are driven in `internal/cookies`, where the write seam lives, and scanned there over the error, `lastError` and the log. All three OS-error answers carry a PATH by design and the scans assert only that no VALUE rides along with it. Nothing scans the reload-failure answer at the wire; its route arm interpolates nothing at all, so it is value-free by construction rather than by test.

**Residual, named rather than hidden (the re-check):** no test drives a real `CheckNow` through the import route. Neither does one exist for the wizard finish or the dashboard refresh, for the reason Arc 10 recorded — `twitchValidateURL` and `youtubeGuideURL` are unexported package vars, so a routes-package test would hit the live network. The behavioural half lives in `internal/cookies`; what is asserted here is that the handler calls it, detached, flushed and gated.

**2. Placeholder scan.** No "TBD", no "add error handling", no "similar to Task N", no "write tests for the above". Every code step carries the code; every test step carries the test and the exact `go test -run` invocation with its expected output. The two places that describe rather than show — Task 4's doc edits and Task 5's table row — show the exact old and new text.

**3. Type consistency.**

- `prepareCookieImport(existing, incoming string) (string, error)` — defined Task 0, called Task 1. Same name, same order, both times.
- `ImportResult{YouTube, Twitch RefreshVerdict; YouTubeAccepted, TwitchAccepted, Wrote bool}` — defined Task 1, read by `cookieImportOutcome` (Task 1) and by the panel's `data.authenticated` / `data.twitchAuthenticated` / `data.youtubeVerification` / `data.twitchVerification` (Task 3) through the wire keys `cookieImportOutcome` emits, which `TestImportOutcomeSpeaksTheSetupOutcomeVocabulary` pins equal to `cookieSetupOutcome`'s. The JS reads exactly those four keys and no fifth.
- `credentialAccepted(p platformAuth) bool` — one definition (Task 1, `autocookies_profile.go`), two callers (`FinishSetupDetailed`, `ImportCookies`).
- `FlagManualRelogin(platform string)` — defined Task 2, called by `runCookieRecovery` (Task 2) and by three tests (Tasks 1's file gains one in Task 2, plus the two routes tests).
- `reloginPromptTarget(status, hostname)` takes both arguments at its definition (Task 3, `utils.js`), at its call site (`answerReloginPrompt`, which passes `window.location.hostname`) and in the test table's `jsCall`. It returns the strings `"wizard"` / `"import"`; the caller compares against `"wizard"` and the table expects both. One spelling each.
- Element ids are written once in `index.html` and read by the same strings in `settings.js` and in the goja probe: `cookie-import-text`, `cookie-import-file`, `btn-cookie-import`, `cookie-import-result`.
- `maxCookieImportBytes` is referenced by the reader, its 413 message and the over-cap test's padding size (60 000 × 10 bytes ≈ 586 KiB > 512 KiB).

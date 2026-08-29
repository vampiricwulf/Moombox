package routes

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// chromiumFamilyOption matches the Web UI's ONE Chromium option in the custom
// browser-type dropdown, and matches it by its LABEL rather than by its value.
//
// Matching on the value would defeat the whole assertion: the value is the
// thing under test, so a pattern that located the option by it could only ever
// find an option that already agrees. The label is the invariant — an option
// offering the Chromium family — and the value is then read off it and
// compared.
var chromiumFamilyOption = regexp.MustCompile(`<sl-option value="([^"]*)">Chromium-family[^<]*</sl-option>`)

// TestChromiumFamilyOptionMatchesTheDpapiSentinel ties three independent
// literals that encode ONE agreement and were tied by nothing.
//
// The three sites:
//
//  1. web/public/index.html — `<sl-option value="chrome">Chromium-family
//     (chrome, brave, edge, vivaldi, thorium, opera)</sl-option>`, the only
//     Chromium choice the Web UI offers. What it stores in cookies.browser_type
//     means "some Chromium browser", never "Google Chrome specifically".
//  2. internal/cookies/autocookies_dpapi.go — DpapiChromiumFamilyValue, the
//     sentinel dpapiExtractAsNetscape compares browser_type against to decide
//     "the operator picked the family, so do not filter by browser at all".
//  3. internal/cookies/browser_validate.go — knownBrowserTypes, which accepts
//     "chrome" alongside the per-browser names the TUI's free-text field can
//     supply. It is what makes the Web UI's value a legal one in the first
//     place, and it is asserted below as the third leg.
//
// THE REGRESSION THIS PREVENTS is one that already happened once (Arc 8 Task 8,
// 25a3370): with the sentinel not firing, a Web UI user who chose the Chromium
// family had every Brave, Edge and Vivaldi profile filtered out of the DPAPI
// scan and got "configured browser has no profiles under LOCALAPPDATA" on a
// machine full of them. Change the option's value to "chromium" and that
// returns silently — the Go constant is still "chrome", nothing compares the
// two, and every Go test stays green because none of them reads the HTML.
//
// STRICTLY ONE MATCH. Zero means the option was renamed or removed and this
// assertion is reading nothing; more than one means a second Chromium option
// appeared, at which point "the value" is not a single fact any more and the
// sentinel's premise — that the UI cannot express a narrower choice — is gone.
func TestChromiumFamilyOptionMatchesTheDpapiSentinel(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")

	matches := chromiumFamilyOption.FindAllStringSubmatch(html, -1)
	switch len(matches) {
	case 1:
	case 0:
		t.Fatalf("no Chromium-family <sl-option> found in index.html. Either the dropdown was " +
			"restructured — in which case this pin is reading nothing and the DPAPI sentinel " +
			"(cookies.DpapiChromiumFamilyValue) is unanchored again — or the Web UI no longer " +
			"offers a Chromium choice at all")
	default:
		t.Fatalf("index.html now has %d Chromium-family options. DpapiChromiumFamilyValue exists "+
			"because the Web UI can express only ONE Chromium choice; with more than one, "+
			"treating the stored value as \"the whole family\" is no longer sound", len(matches))
	}

	got := matches[0][1]
	if got != cookies.DpapiChromiumFamilyValue {
		t.Errorf("the Chromium-family option stores browser_type=%q, but the DPAPI fallback's "+
			"sentinel is %q. They must be identical: dpapiExtractAsNetscape compares the stored "+
			"value against the sentinel to decide NOT to filter by browser, and with them apart "+
			"every Brave/Edge/Vivaldi profile is filtered out of the scan on a machine that has "+
			"them. That is the exact regression 25a3370 fixed, and it fails silently — no Go test "+
			"other than this one reads the HTML",
			got, cookies.DpapiChromiumFamilyValue)
	}

	// The third leg. The stored value also has to survive
	// ValidateBrowserPathQuick's knownBrowserTypes, or a custom path saved from
	// the Web UI is rejected before the DPAPI question is ever asked. The path
	// is absolute and does not exist, which is deliberate: it clears the two
	// checks that precede the type check and then fails at the stat, so the
	// only verdict this can read is the one about the TYPE.
	probePath := filepath.Join(t.TempDir(), "browser-that-does-not-exist")
	if err := cookies.ValidateBrowserPathQuick(probePath, got); err != nil &&
		strings.Contains(err.Error(), "unknown browser type") {
		t.Errorf("browser_type=%q from the Web UI's own dropdown is not in knownBrowserTypes: %v. "+
			"The dropdown would be offering a value the validator refuses, and the custom path "+
			"could never be saved", got, err)
	}
}

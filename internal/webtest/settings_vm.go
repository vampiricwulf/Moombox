// Package webtest holds Go-test-only helpers shared across packages that need
// to run the shipped Web UI JavaScript modules against a goja runtime.
//
// It exists because internal/tui and internal/web/routes cannot see each
// other's *_test.go helpers, and the rule for evaluating settings.js — utils.js
// first, with its own import and every `export` stripped — used to be
// duplicated between them as a doc comment. A third harness once carried the
// rule slightly differently and re-broke it (Arc B). SettingsVM is the one
// place the rule now lives; it is not a _test.go file so both packages can
// import it.
package webtest

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// utilsImportRe matches the ES import settings.js uses to pull in utils.js.
// goja does not parse ES module syntax, so the line has to go before either
// file reaches RunString. settings.js imports only "./utils.js" — never add a
// second import to settings.js, or this harness silently stops stripping it.
var utilsImportRe = regexp.MustCompile(`(?s)import \{[^}]*\} from "\./utils\.js";`)

// SettingsVM returns a goja runtime with the SHIPPED utils.js and settings.js
// evaluated, in that order, from the embedded web assets.
//
// The only transforms are stripping the ES `import` statement and the
// `export` keyword, neither of which goja parses; nothing else is rewritten,
// so what runs here is the source the binary serves. Line endings are
// normalised first because web/public is a mix of CRLF and LF.
//
// settings.js imports only ./utils.js; never add a second import — this
// harness strips exactly that one line. A second import survives into the
// source goja is handed and every test that calls this fails on a parse error
// with no hint of why, so the rule is stated where a caller reads it and again
// on utilsImportRe where the pattern lives.
//
// utils.js is evaluated FIRST so the helpers settings.js imports are ordinary
// global function declarations by the time settings.js is parsed — without
// them every reference is a ReferenceError, which inside a method's own
// try/catch renders as a handled error (no toast, empty failure) rather than
// a visible one, and a correct implementation would read as broken.
//
// This is the ONE way to evaluate settings.js in Go tests. It replaces two
// near-identical harnesses (`settingsVM` in internal/tui,
// `settingsPanelVM` in internal/web/routes) that carried this rule as a doc
// comment instead of code because the two packages could not share a
// _test.go helper.
func SettingsVM(t testing.TB) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	for _, mod := range []string{"public/modules/utils.js", "public/modules/settings.js"} {
		raw, err := webassets.PublicFS.ReadFile(mod)
		if err != nil {
			t.Fatalf("read the embedded %s: %v", mod, err)
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		src = utilsImportRe.ReplaceAllString(src, "")
		src = strings.ReplaceAll("\n"+src, "\nexport ", "\n")
		if _, err := vm.RunString(src); err != nil {
			t.Fatalf("%s does not evaluate — the browser would fail the same way: %v", mod, err)
		}
	}
	return vm
}

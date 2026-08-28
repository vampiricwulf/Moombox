package tui

import (
	"strings"
	"testing"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// restartFieldsFromJS runs the SHIPPED settings.js module and reads
// RESTART_REQUIRED_FIELDS back out of it.
//
// Executed rather than pattern-matched, for the reason
// cookies_setup_utilsvm_test.go gives in internal/web/routes: a source match on
// a JS list passes on a list that is malformed, commented out, or shadowed
// three lines later. What the browser gets is what evaluates, so that is what
// is asserted. The only transforms are stripping the ES `import` statement and
// the `export` keyword, neither of which goja parses; nothing else is rewritten.
//
// The module is DOM-coupled, but only inside methods — the class body and the
// two module-level constants evaluate with no document at all.
func restartFieldsFromJS(t *testing.T) []map[string]any {
	t.Helper()

	vm := settingsVM(t)
	// RESTART_REQUIRED_FIELDS is a module-level `const`, which lives in the
	// global lexical environment rather than on the global object, so it is
	// read as the completion value of an expression rather than via vm.Get.
	v, err := vm.RunString("RESTART_REQUIRED_FIELDS")
	if err != nil {
		t.Fatalf("settings.js no longer defines RESTART_REQUIRED_FIELDS: %v", err)
	}
	rows, ok := v.Export().([]any)
	if !ok {
		t.Fatalf("RESTART_REQUIRED_FIELDS is %T, want an array", v.Export())
	}
	out := make([]map[string]any, 0, len(rows))
	for i, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("RESTART_REQUIRED_FIELDS[%d] is %T, want an object", i, row)
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		t.Fatal("RESTART_REQUIRED_FIELDS is empty — nothing below can be concluded")
	}
	return out
}

// TestRestartRequiredListsAgree pins the two lists against each other.
//
// They are one fact written twice, in two languages, with nothing connecting
// them: internal/tui's restartRequiredKeys and settings.js's
// RESTART_REQUIRED_FIELDS. Whichever UI is edited alone keeps working perfectly
// while the other silently stops warning, and the failure is invisible — the
// operator saves, sees no prompt, and concludes the setting took effect.
//
// Compared as SETS in both directions, so neither list can grow or shrink
// alone.
func TestRestartRequiredListsAgree(t *testing.T) {
	// The TUI keys on a bare field name; the Web on the config's nested path.
	// The last segment is the join, and it is unambiguous because every TUI key
	// is unique across sections (that uniqueness is what makes
	// restartRequiredKeys[fd.key] a valid lookup in the first place).
	web := map[string]string{} // bare key -> full path, for the error messages
	for _, row := range restartFieldsFromJS(t) {
		path, _ := row["path"].(string)
		if path == "" || !strings.Contains(path, ".") {
			t.Fatalf("RESTART_REQUIRED_FIELDS has an entry with no nested path: %v", row)
		}
		key := path[strings.LastIndex(path, ".")+1:]
		if prev, dup := web[key]; dup {
			t.Errorf("two Web entries collapse onto the TUI key %q (%q and %q), so the comparison "+
				"below cannot be a bijection", key, prev, path)
		}
		web[key] = path
	}

	for key := range restartRequiredKeys {
		if _, ok := web[key]; !ok {
			t.Errorf("the TUI warns that %q needs a restart and the Web dashboard does not — a user who "+
				"changes it there saves, sees no prompt, and believes it took effect", key)
		}
	}
	for key, path := range web {
		if !restartRequiredKeys[key] {
			t.Errorf("the Web dashboard warns that %q (%q) needs a restart and the TUI does not", path, key)
		}
	}
}

// TestRestartRequiredKeysAreRealFields is the premise the test above rests on.
//
// restartRequiredKeys is consulted as `restartRequiredKeys[fd.key]` while
// rendering, so a key that names no field is never read: it warns about
// nothing, and it would satisfy the parity check above just as well as a real
// one. The reverse direction is not asserted — most fields are correctly absent
// from the map.
func TestRestartRequiredKeysAreRealFields(t *testing.T) {
	known := map[string]bool{}
	for _, sec := range sections {
		for _, fd := range sec.fields {
			if known[fd.key] {
				t.Errorf("duplicate settings key %q — restartRequiredKeys is keyed on the bare name, "+
					"so two fields sharing one would be flagged together", fd.key)
			}
			known[fd.key] = true
		}
	}
	for key := range restartRequiredKeys {
		if !known[key] {
			t.Errorf("restartRequiredKeys names %q, which is not a field in any section — nothing reads "+
				"it, so it warns about nothing", key)
		}
	}
}

// TestRestartRequiredWebIdsExist pins the half of the Web entry that fails
// SILENTLY.
//
// `path` drives the restart prompt and a wrong one is at least visible as a
// missing prompt. `id` drives nothing but the "Restart" badge, inserted with
// `document.getElementById(id)` followed by `if (!el) continue` — so a typo,
// a renamed element or a copied-from-the-wrong-row id produces no error, no
// console warning and no badge. The three cookie ids added here are exactly
// where that is easiest to get wrong: they do not follow the `cfg-<key>`
// pattern the network rows use (`cookies.auto_enabled` is `cfg-auto-cookies-
// enabled`, not `cfg-auto-enabled`).
//
// Checked against the shipped index.html, which is the document the code runs
// against, and both attribute quotings are accepted so the assertion is about
// the markup rather than its formatting.
//
// EXISTENCE ONLY, and that is not the whole guard. Repointing a row at a
// different element that also exists satisfies everything here while the badge
// renders beside a control the user is not editing — a mutation run confirmed
// this test survives exactly that swap. Correspondence is measured separately,
// by running the shipped populate code: see
// TestRestartBadgeIdsAreTheControlsTheirPathsDrive. The two are kept apart
// because they fail for different reasons and name different fixes — a typo'd
// id that matches nothing, versus a real id on the wrong row.
func TestRestartRequiredWebIdsExist(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/index.html")
	if err != nil {
		t.Fatalf("read the embedded index.html: %v", err)
	}
	html := string(raw)

	for _, row := range restartFieldsFromJS(t) {
		id, _ := row["id"].(string)
		path, _ := row["path"].(string)
		if id == "" {
			t.Errorf("the entry for %q carries no element id, so it can never render a badge", path)
			continue
		}
		if !strings.Contains(html, `id="`+id+`"`) && !strings.Contains(html, `id='`+id+`'`) {
			t.Errorf("RESTART_REQUIRED_FIELDS points %q at element %q, which does not exist in "+
				"index.html — getElementById returns null, the code skips it, and the operator "+
				"never sees the Restart badge", path, id)
		}
	}
}

// TestRestartOverlayNamesEveryCategoryItCovers guards the sentence the operator
// actually reads.
//
// Both UIs summarise the list as a handful of CATEGORIES rather than enumerate
// twelve keys, and that summary went stale the moment the cookie settings were
// added: an operator who changed only a cookie setting was shown a prompt
// naming port, network access, database path and log settings — four things
// they had not touched — which reads as a prompt about something else and gets
// dismissed. That dismissal is the exact failure the entries were added to
// prevent.
//
// The TUI half is asserted on the RENDERED overlay. The Web half is asserted on
// the confirm text extracted from the shipped module: it lives inside a
// DOM-coupled method, so it cannot be executed here, but the literal is
// extracted by its own opening rather than searched for file-wide.
func TestRestartOverlayNamesEveryCategoryItCovers(t *testing.T) {
	// The categories the twelve keys fall into, each paired with a word the
	// summary must contain. Derived from restartRequiredKeys rather than
	// listed, so a future key in a fifth category fails here instead of
	// quietly widening the gap between the list and the sentence.
	categoryOf := map[string]string{
		"port": "network", "network_access": "network", "https_enabled": "network",
		"tls_cert_path": "network", "tls_key_path": "network",
		"database_path": "database", "log_file_path": "log",
		"log_max_file_size": "log", "log_max_files": "log",
		"cookie_file": "cookie", "auto_enabled": "cookie", "browser_profile_dir": "cookie",
	}
	var wanted []string
	seen := map[string]bool{}
	for key := range restartRequiredKeys {
		word, ok := categoryOf[key]
		if !ok {
			t.Fatalf("restartRequiredKeys gained %q and this test does not know which category the "+
				"restart prompts put it in — add it above, and add it to both prompts", key)
		}
		if !seen[word] {
			seen[word] = true
			wanted = append(wanted, word)
		}
	}

	m := NewSettingsModel()
	m.width, m.height = 100, 30
	tuiPrompt := m.renderRestartOverlay()

	webPrompt := restartConfirmText(t)

	for _, surface := range []struct{ what, got string }{
		{"the TUI restart overlay", tuiPrompt},
		{"the Web restart confirm", webPrompt},
	} {
		lower := strings.ToLower(surface.got)
		for _, word := range wanted {
			if !strings.Contains(lower, word) {
				t.Errorf("%s does not mention %q, although a %s setting triggers it — the operator is "+
					"shown a list naming nothing they changed: %q", surface.what, word, word, surface.got)
			}
		}
	}
}

// restartConfirmText extracts the Web restart prompt's sentence from the
// shipped module by its own opening, so the assertion names the site rather
// than the file. A file-wide search would be satisfied by any other string that
// happened to mention the same words.
func restartConfirmText(t *testing.T) string {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile("public/modules/settings.js")
	if err != nil {
		t.Fatalf("read the embedded settings.js: %v", err)
	}
	const opening = `"Some settings require a restart to take effect (`
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	at := strings.Index(src, opening)
	if at < 0 {
		t.Fatalf("settings.js no longer opens its restart prompt with %q — if it was reworded, reword "+
			"this extraction with it rather than deleting the assertion", opening)
	}
	rest := src[at+len(opening):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatal("the Web restart prompt's category list is unterminated")
	}
	return rest[:end]
}

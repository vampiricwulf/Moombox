package routes

import (
	"strings"
	"testing"
)

// TestReorderSettingsControlsExist pins the dashboard half: the two controls
// have to be in the EMBEDDED markup (a hand-edited web/public that was never
// rebuilt ships nothing), they have to accept 0, and settings.js has to both
// populate them and send them.
//
// MUTANT: min="1" on either input — the browser refuses the documented
// "unbounded" value before the request is ever made. MUTANT: add the inputs
// but forget the payload keys in saveConfig — the field renders, the operator
// edits it, the save reports success and nothing changes.
func TestReorderSettingsControlsExist(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	js := readEmbeddedModule(t, "public/modules/settings.js")

	for _, tc := range []struct{ id, key string }{
		{"cfg-reorder-buffer-mb", "reorder_buffer_mb"},
		{"cfg-reorder-budget-mb", "reorder_budget_mb"},
	} {
		idx := strings.Index(html, `id="`+tc.id+`"`)
		if idx < 0 {
			t.Errorf("index.html has no %s control — the setting is unreachable from the dashboard", tc.id)
			continue
		}
		// The element's own attribute run: from the opening tag before the id
		// to the next ">".
		start := strings.LastIndex(html[:idx], "<")
		end := strings.Index(html[idx:], ">")
		if start < 0 || end < 0 {
			t.Fatalf("%s: could not delimit the element", tc.id)
		}
		el := html[start : idx+end]
		if !strings.Contains(el, `type="number"`) {
			t.Errorf("%s is not a number input: %s", tc.id, el)
		}
		if !strings.Contains(el, `min="0"`) {
			t.Errorf("%s does not carry min=\"0\" — 0 is the documented unbounded value: %s", tc.id, el)
		}
		if !strings.Contains(el, "0 = unbounded") {
			t.Errorf("%s has no help text naming the unbounded value: %s", tc.id, el)
		}

		if !strings.Contains(js, tc.id) {
			t.Errorf("settings.js never mentions %s — the field is never populated or read", tc.id)
		}
		if !strings.Contains(js, tc.key+":") {
			t.Errorf("settings.js never puts %s in the save payload — an edit to that field is discarded", tc.key)
		}
	}
}

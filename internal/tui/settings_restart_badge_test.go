package tui

import (
	"encoding/json"
	"testing"

	"github.com/dop251/goja"
)

// TestRestartBadgeIdsAreTheControlsTheirPathsDrive closes a guard gap a
// mutation run found: the ids were pinned for EXISTENCE, not correspondence.
//
// RESTART_REQUIRED_FIELDS carries {path, id} pairs. `path` drives the restart
// prompt; `id` is the element the "Restart" badge is inserted after, via
// `getElementById(id)` followed by `if (!el) continue`. Pointing a row at a
// different element that happens to exist therefore costs nothing visible — no
// error, no console warning, just a badge that silently never renders on the
// control the user is editing, and renders on one they are not. Checking that
// each id exists cannot see it, which is this plan's "a name-based check is no
// guard" rule applied to the id half.
//
// So correspondence is measured, by DIFFERENTIAL PROBING of the shipped
// populate code: run populateConfigForm with an empty config, run it again with
// exactly one config path set, and take the ids whose recorded value changed.
// Those are the controls that path drives, derived from the module itself
// rather than from the list under test — so the assertion cannot be satisfied
// by the list agreeing with itself. Exactly one must change, and it must be the
// paired id.
//
// The probe value is chosen per type so "changed" is unambiguous: a sentinel
// string for text and numeric fields, and `true` for switches, which the
// baseline leaves false.
func TestRestartBadgeIdsAreTheControlsTheirPathsDrive(t *testing.T) {
	vm := settingsVM(t)
	if _, err := vm.RunString(populateProbe); err != nil {
		t.Fatalf("install the populate probe: %v", err)
	}

	baseline := runPopulate(t, vm, map[string]any{})

	// Most restart-required fields are plain text or number inputs, where any
	// string that differs from the empty baseline is a valid probe. Three
	// controls need a specific override so "changed" stays unambiguous:
	//
	//   - https_enabled and auto_enabled read `config.x?.y === true`; the
	//     baseline (unset) is false, so only a literal true moves them.
	//   - use_sidecar reads the opposite way, `!== false`, and so defaults to
	//     true when unset — a literal false is the only value that moves it
	//     off that baseline.
	//   - probe_targets is rendered with `.join(", ")`; a bare string sentinel
	//     has no such method, so its probe must already be an array.
	probeOverrides := map[string]any{
		"network.https_enabled":      true,
		"cookies.auto_enabled":       true,
		"bgutils.use_sidecar":        false,
		"connectivity.probe_targets": []any{"moombox-restart-probe-connectivity.probe_targets"},
	}

	for _, row := range restartFieldsFromJS(t) {
		path, _ := row["path"].(string)
		id, _ := row["id"].(string)

		t.Run(path, func(t *testing.T) {
			probeValue, overridden := probeOverrides[path]
			if !overridden {
				probeValue = "moombox-restart-probe-" + path
			}

			seen := runPopulate(t, vm, nestedConfig(path, probeValue))

			var changed []string
			for k, v := range seen {
				if !sameJSON(t, baseline[k], v) {
					changed = append(changed, k)
				}
			}
			// A path that changes nothing means populateConfigForm never reads
			// it, so the badge can only ever be on the wrong control — and the
			// existence check would still pass.
			if len(changed) == 0 {
				t.Fatalf("setting %q changed no control at all, so nothing in the form is driven by "+
					"it and %q cannot be the right badge target", path, id)
			}
			if len(changed) != 1 || changed[0] != id {
				t.Errorf("%q drives %v, but RESTART_REQUIRED_FIELDS puts its Restart badge on %q. "+
					"The badge renders beside a control the user is not editing, and never beside "+
					"the one they are — with no error anywhere, because getElementById found "+
					"something", path, changed, id)
			}
		})
	}
}

// sameJSON compares two probe values structurally. Values cross the goja
// boundary as any, so a JSON round trip is the cheapest way to compare
// undefined/absent against a real value without asserting a Go type.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal probe value %v: %v", a, err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal probe value %v: %v", b, err)
	}
	return string(ja) == string(jb)
}

// TestPopulateProbeSeesTheFormAtAll is the premise every row above rests on.
//
// If populateConfigForm threw early, or the stub DOM swallowed every write, the
// differential check would find "no control changed" for each path and the
// table would be all failures — but a subtler breakage (the probe recording
// nothing while the method ran) would make `changed` empty everywhere, which is
// already fatal above. What this guards is the opposite direction: that the
// probe is observing a REAL form with many controls, not one lucky element.
func TestPopulateProbeSeesTheFormAtAll(t *testing.T) {
	vm := settingsVM(t)
	if _, err := vm.RunString(populateProbe); err != nil {
		t.Fatalf("install the populate probe: %v", err)
	}
	seen := runPopulate(t, vm, map[string]any{})
	if len(seen) < len(restartFieldsFromJS(t)) {
		t.Fatalf("populateConfigForm touched only %d controls, which is fewer than the restart list "+
			"alone — the probe is not observing the real form", len(seen))
	}
	if _, ok := goja.AssertFunction(vm.Get("__probeConfigForm")); !ok {
		t.Error("the probe function vanished")
	}
}

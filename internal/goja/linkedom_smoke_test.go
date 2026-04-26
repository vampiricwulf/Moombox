package goja

import (
	"context"
	"os"
	"testing"
)

// TestLinkedomBundleSmoke loads the linkedom ESM bundle into our shimmed
// runtime and exercises its parseHTML entry point. Skipped unless the
// bundle exists on disk (the build product lives under
// references/happy-dom-build/dist/, which is gitignored). Run via:
//
//	cd references/happy-dom-build && npm install && npm run build:linkedom
//	cd ../.. && go test -v -run TestLinkedomBundleSmoke ./internal/goja/...
func TestLinkedomBundleSmoke(t *testing.T) {
	bundlePath := "../../references/happy-dom-build/dist/linkedom.bundle.js"
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Skipf("linkedom bundle not built; run `npm run build` in references/happy-dom-build first (%v)", err)
	}

	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}

	if _, err := vm.RunString(string(bundle)); err != nil {
		t.Fatalf("loading linkedom bundle: %v", err)
	}

	res, err := vm.RunString(`(function() {
		var dom = LinkeDOM.parseHTML('<!doctype html><html><head><title>t</title></head><body></body></html>');
		return JSON.stringify({
			documentToString: Object.prototype.toString.call(dom.document),
			navigatorUA: dom.navigator && dom.navigator.userAgent,
			locationHref: dom.location && dom.location.href,
			windowSelfEq: dom.window === dom.window.self,
			documentInstanceofDocument: dom.document instanceof dom.Document,
			navigatorInstanceofNavigator: dom.navigator instanceof dom.Navigator,
			elementToString: Object.prototype.toString.call(dom.document.body),
		});
	})()`)
	if err != nil {
		t.Fatalf("linkedom smoke probe: %v", err)
	}
	t.Logf("linkedom dump: %s", res.String())
}

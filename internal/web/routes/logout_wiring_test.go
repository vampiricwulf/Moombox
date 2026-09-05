package routes

import (
	"strings"
	"testing"
)

// TestLogoutIconWiringIsPinned pins the pieces of the status-bar logout icon
// that modules/logout.js's own unit tests cannot see: the markup, the CSS
// that makes it a sibling of the theme toggle, and the two call sites in
// app.js/settings.js that show and hide it. A mutant that drops any one of
// them keeps every node test green; this test is what fails.
func TestLogoutIconWiringIsPinned(t *testing.T) {
	appJS := readEmbeddedModule(t, "public/app.js")
	settingsJS := readEmbeddedModule(t, "public/modules/settings.js")
	html := readEmbeddedModule(t, "public/index.html")
	css := readEmbeddedModule(t, "public/moombox.css")

	if !strings.Contains(appJS, `import { applyLogoutVisibility, bindLogout } from "./modules/logout.js";`) {
		t.Error("app.js does not import applyLogoutVisibility/bindLogout from ./modules/logout.js")
	}
	if body := jsMethodBody(t, appJS, "checkSecurityBanner"); !strings.Contains(body, "this.applyAuthStatus(status)") {
		t.Error("checkSecurityBanner no longer routes the auth status through applyAuthStatus")
	}
	// applyAuthStatus takes an argument, so jsMethodBody's zero-argument
	// headers cannot name it — jsMethodBodyAt brackets it by exact header.
	if body := jsMethodBodyAt(t, appJS, "\n  applyAuthStatus(status) {"); !strings.Contains(body, `applyLogoutVisibility(document.getElementById("btn-logout")`) {
		t.Error("applyAuthStatus no longer syncs the logout icon")
	}
	if !strings.Contains(appJS, `bindLogout(document.getElementById("btn-logout")`) {
		t.Error("app.js no longer binds the logout click")
	}
	if body := jsMethodBody(t, settingsJS, "loadSecurityStatus"); !strings.Contains(body, "applyAuthStatus") {
		t.Error("settings.js loadSecurityStatus no longer re-syncs the logout icon after set/remove password")
	}

	right := strings.Index(html, `id="status-bar-right"`)
	logout := strings.Index(html, `id="btn-logout"`)
	theme := strings.Index(html, `id="theme-toggle"`)
	if right < 0 || logout < 0 || theme < 0 || !(right < logout && logout < theme) {
		t.Errorf("#btn-logout must sit inside #status-bar-right before #theme-toggle (indexes right=%d logout=%d theme=%d)", right, logout, theme)
	}
	if strings.Count(html, `id="btn-logout"`) != 1 {
		t.Error("exactly one #btn-logout expected")
	}
	tag := html[logout:]
	tag = tag[:strings.Index(tag, ">")+1]
	for _, want := range []string{`name="box-arrow-right"`, `style="display:none"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("#btn-logout tag lacks %s: %s", want, tag)
		}
	}

	for _, want := range []string{
		"#theme-toggle, #btn-logout {",
		"#theme-toggle::part(base), #btn-logout::part(base) {",
		"#theme-toggle:hover, #btn-logout:hover {",
		":not(#theme-toggle):not(#btn-logout)::part(base)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("moombox.css lacks the logout rule %q — the icon would render unstyled beside the theme toggle", want)
		}
	}
}

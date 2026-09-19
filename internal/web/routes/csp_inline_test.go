package routes

import (
	"regexp"
	"strings"
	"testing"
)

// inlineScriptRe matches a <script> tag that has no src attribute — i.e. one
// whose body is inline JavaScript, which `script-src 'self'` refuses.
var inlineScriptRe = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`)

// inlineHandlerRe matches an inline event-handler attribute (onclick=,
// onchange=, onerror=, …). The browser treats each one as inline script, so a
// single one of these is refused under `script-src 'self'` just as an inline
// <script> is — silently, with no console-free way to notice from Go.
var inlineHandlerRe = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

// cspPages are the two documents the browser ever loads directly. Every other
// asset under web/public is fetched BY one of them.
var cspPages = []string{"public/index.html", "public/login.html"}

// TestNoPageCarriesAnInlineScript is the other half of the WEB-10 pin: the CSP
// test proves the header no longer allows inline script; this proves no page
// still needs it. A page that regains one is silently broken in the browser —
// the CSP refuses it and nothing in Go notices.
//
// THE MUTANT: put the theme bootstrap back inline in either page. The count of
// src-less <script> tags goes above zero and the page's theme (or its login
// form) stops working under the shipped CSP.
func TestNoPageCarriesAnInlineScript(t *testing.T) {
	for _, page := range cspPages {
		html := readEmbeddedModule(t, page)
		for _, tag := range inlineScriptRe.FindAllString(html, -1) {
			if !strings.Contains(strings.ToLower(tag), " src=") {
				t.Errorf("%s carries an inline <script>: %q — script-src is 'self' only, so the browser "+
					"refuses to run it", page, tag)
			}
		}
		if !strings.Contains(html, `src="/boot-theme.js"`) {
			t.Errorf("%s does not load /boot-theme.js — its theme bootstrap has to come from somewhere", page)
		}
	}
	if login := readEmbeddedModule(t, "public/login.html"); !strings.Contains(login, `src="/login.js"`) {
		t.Error("login.html does not load /login.js — the form would never wire up")
	}
	// Both files must actually exist in the embedded FS, or the pages 404 them.
	for _, f := range []string{"public/boot-theme.js", "public/login.js"} {
		if readEmbeddedModule(t, f) == "" {
			t.Errorf("%s is empty or missing from the embedded assets", f)
		}
	}
}

// TestNoPageCarriesAnInlineEventHandler is the same pin for the other kind of
// inline script. An onclick= attribute needs `script-src 'unsafe-inline'`
// every bit as much as a <script> block does, and neither page has one today.
//
// THE MUTANT: wire a control with onclick="…" instead of addEventListener.
// The button does nothing in a browser and every Go test stays green.
func TestNoPageCarriesAnInlineEventHandler(t *testing.T) {
	for _, page := range cspPages {
		html := readEmbeddedModule(t, page)
		for _, m := range inlineHandlerRe.FindAllStringIndex(html, -1) {
			// Report the attribute with a little context so the site is findable.
			start := m[0]
			if start < 0 {
				start = 0
			}
			end := m[1] + 40
			if end > len(html) {
				end = len(html)
			}
			t.Errorf("%s carries an inline event handler: %q — script-src is 'self' only, so the browser "+
				"refuses to run it", page, strings.TrimSpace(html[start:end]))
		}
	}
}

// bootThemeTagRe isolates the <script> element that loads the theme bootstrap.
var bootThemeTagRe = regexp.MustCompile(`(?is)<script[^>]*src="/boot-theme\.js"[^>]*>`)

// TestBootThemeScriptIsParserBlockingAfterTheThemeLinks pins the two placement
// facts the merged bootstrap depends on.
//
// It replaced TWO inline blocks: one before the Shoelace theme <link> tags
// that set the root class, and one after them that swapped which stylesheet
// was active. One file can do both ONLY if it is parser-blocking (no defer, no
// async, not a module — all three of which run after the parser has moved on,
// so the page paints dark first and flashes) AND placed after both <link>
// tags, so getElementById finds them.
//
// THE MUTANT: mark the tag `defer` (or `type="module"`), or move it above the
// <link> tags. Either way a light-theme user gets a dark flash on every load
// and the light stylesheet may never activate at all.
func TestBootThemeScriptIsParserBlockingAfterTheThemeLinks(t *testing.T) {
	for _, page := range cspPages {
		html := readEmbeddedModule(t, page)

		tag := bootThemeTagRe.FindString(html)
		if tag == "" {
			t.Errorf("%s has no <script src=\"/boot-theme.js\"> element", page)
			continue
		}
		lower := strings.ToLower(tag)
		for _, bad := range []string{" defer", " async", `type="module"`} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s: the boot-theme tag is %q — %q makes it run after the parser has already "+
					"painted, which is the FOUC this script exists to prevent", page, tag, strings.TrimSpace(bad))
			}
		}

		at := strings.Index(html, tag)
		for _, id := range []string{`id="sl-theme-dark"`, `id="sl-theme-light"`} {
			linkAt := strings.Index(html, id)
			if linkAt < 0 {
				t.Errorf("%s has no %s stylesheet link", page, id)
				continue
			}
			if linkAt > at {
				t.Errorf("%s: /boot-theme.js is loaded BEFORE the %s link — the swap half of the script "+
					"calls getElementById on an element the parser has not reached yet", page, id)
			}
		}
	}
}

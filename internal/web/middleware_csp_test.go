package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The chat replay renders BTTV / 7TV / FFZ emotes as <img> from their CDNs
// (internal/twitch/emotes.go). A CSP img-src that omits a host makes the
// browser refuse the image silently and the player falls back to the emote
// code as text — the whole emote pipeline looked dead in the field.
func TestSecurityHeadersCSPAdmitsEmoteCDNs(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	imgSrc := ""
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "img-src ") {
			imgSrc = d
		}
	}
	if imgSrc == "" {
		t.Fatal("CSP has no img-src directive")
	}
	for _, host := range []string{
		"https://cdn.betterttv.net", "https://cdn.7tv.app", "https://cdn.frankerfacez.com",
		"https://*.jtvnw.net", "https://yt3.ggpht.com", "https://i.ytimg.com",
	} {
		if !strings.Contains(imgSrc, " "+host) {
			t.Errorf("img-src lacks %s: %q", host, imgSrc)
		}
	}
}

// TestSecurityHeadersCSPHasNoInlineScript is the WEB-10 pin.
//
// script-src carried 'unsafe-inline' for the theme-bootstrap and login
// scripts, which disabled the CSP as a second line of defence behind ~77
// escaped innerHTML sinks. style-src KEEPS it — Shoelace's shadow DOM and its
// dynamically generated styles need it, and that is a different risk.
//
// THE MUTANT: put 'unsafe-inline' back in script-src (or, equally, delete it
// from style-src and break every Shoelace component).
func TestSecurityHeadersCSPHasNoInlineScript(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	directives := map[string]string{}
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		d = strings.TrimSpace(d)
		if name, rest, ok := strings.Cut(d, " "); ok {
			directives[name] = rest
		}
	}

	script, ok := directives["script-src"]
	if !ok {
		t.Fatal("CSP has no script-src directive")
	}
	if strings.Contains(script, "'unsafe-inline'") {
		t.Errorf("script-src still allows inline script: %q — the whole point of moving the boot scripts "+
			"to files was to remove it", script)
	}
	if !strings.Contains(script, "'self'") || !strings.Contains(script, "https://cdn.jsdelivr.net") {
		t.Errorf("script-src must still admit 'self' and the Shoelace CDN: %q", script)
	}
	if style := directives["style-src"]; !strings.Contains(style, "'unsafe-inline'") {
		t.Errorf("style-src must KEEP 'unsafe-inline' for Shoelace's shadow DOM: %q", style)
	}
}

// TestSecurityHeadersCSPIsByteForByte pins the WHOLE header, not just the one
// directive WEB-10 changes.
//
// The directive-level test above cannot see collateral damage: an edit that
// drops a host from img-src, or loosens object-src, while tightening
// script-src would leave it green. Every other directive here is unchanged
// from before WEB-10; the only edit is script-src losing 'unsafe-inline'.
//
// THE MUTANT: any edit to any directive — including putting 'unsafe-inline'
// back in script-src — makes this diff.
func TestSecurityHeadersCSPIsByteForByte(t *testing.T) {
	const want = "default-src 'self'; " +
		"script-src 'self' https://cdn.jsdelivr.net; " +
		"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " +
		"font-src 'self' https://cdn.jsdelivr.net; " +
		"img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net " +
		"https://*.ttvnw.net https://cdn.betterttv.net https://cdn.7tv.app https://cdn.frankerfacez.com " +
		"https://cdn.jsdelivr.net https://fonts.gstatic.com; " +
		"connect-src 'self' ws: wss: https://cdn.jsdelivr.net data:; " +
		"frame-src https://www.youtube-nocookie.com https://player.twitch.tv; " +
		"object-src 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"

	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if got := rec.Header().Get("Content-Security-Policy"); got != want {
		t.Errorf("CSP changed\n got: %s\nwant: %s", got, want)
	}
}

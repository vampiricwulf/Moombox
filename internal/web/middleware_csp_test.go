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

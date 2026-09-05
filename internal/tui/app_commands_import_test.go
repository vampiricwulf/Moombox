package tui

import (
	"net/url"
	"strings"
	"testing"
)

// TestNewImportRequestEncodesHeaders: the server PathUnescapes these headers
// (import_routes.go decodeImportHeader) and the Web UI encodeURIComponent()s
// them; the TUI sent them raw, so a '%' was mangled and non-Latin-1 titles
// went out as raw UTF-8 in an HTTP header.
func TestNewImportRequestEncodesHeaders(t *testing.T) {
	title := " 50% off / 春のライブ+1 "
	channel := "Some Channel"
	req, err := newImportRequest("http://127.0.0.1:774", strings.NewReader("zip"), title, channel)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.String() != "http://127.0.0.1:774/api/import" {
		t.Errorf("request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	h := req.Header.Get("X-Import-Title")
	if strings.ContainsAny(h, " %春") && !strings.Contains(h, "%25") {
		t.Errorf("title header not percent-encoded: %q", h)
	}
	for _, r := range h {
		if r > 0x7e {
			t.Fatalf("non-ASCII byte in header: %q", h)
		}
	}
	back, err := url.PathUnescape(h)
	if err != nil || back != strings.TrimSpace(title) {
		t.Errorf("round trip = %q (%v), want %q", back, err, strings.TrimSpace(title))
	}
	if got := req.Header.Get("X-Import-Channel"); got != url.PathEscape(channel) {
		t.Errorf("channel header = %q, want %q", got, url.PathEscape(channel))
	}
}

func TestNewImportRequestOmitsEmptyHeaders(t *testing.T) {
	req, err := newImportRequest("http://127.0.0.1:774", strings.NewReader(""), "  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Header["X-Import-Title"]; ok {
		t.Error("blank title must not set X-Import-Title")
	}
	if _, ok := req.Header["X-Import-Channel"]; ok {
		t.Error("empty channel must not set X-Import-Channel")
	}
}

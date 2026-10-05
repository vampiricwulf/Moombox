package tui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// An archive import with no YouTube id is minted "imp_" + eight hex digits,
// and its URL is built from that — a watch page for no video at all. The
// dashboard hides it (isImportPlaceholderId); O S opened it and O C copied it.
// An eleven-character id that merely starts "imp_" is a real YouTube id and
// keeps its page.
//
// Mutant: dropping the placeholder check from streamURL, or from the details
// panel's Stream URL row.
func TestImportPlaceholderHasNoStreamURL(t *testing.T) {
	ph := &database.Job{ID: "imp_0123abcd", VideoID: "imp_0123abcd", Platform: "youtube",
		URL: "https://www.youtube.com/watch?v=imp_0123abcd", Status: database.StatusFinished, Title: "t"}
	if got := streamURL(ph); got != "" {
		t.Errorf("streamURL(placeholder) = %q, want none", got)
	}
	if canOpenStream(ph) {
		t.Error("O S / O C are offered for an import placeholder")
	}
	m := NewJobDetailsModel()
	m.SetSize(80, 40)
	m.SetJob(ph)
	for _, r := range m.rows {
		if r.label == "Stream URL" {
			t.Errorf("the details panel shows the placeholder's URL %q", r.value)
		}
	}

	real := &database.Job{VideoID: "imp_abcdefg", Platform: "youtube"} // 11 chars: a real id
	if got := streamURL(real); got != "https://www.youtube.com/watch?v=imp_abcdefg" {
		t.Errorf("streamURL(real id starting imp_) = %q", got)
	}
}

// canOpenStream is exactly "streamURL has one": a Twitch channel job with no
// channel name has no page, and the menu offered O S for it anyway, after
// which the chord answered "No stream URL available".
//
// Mutant: canOpenStream returning j.URL != "" || j.VideoID != "" again.
func TestCanOpenStreamAgreesWithStreamURL(t *testing.T) {
	j := &database.Job{VideoID: "tw_123", Platform: "twitch"}
	if canOpenStream(j) {
		t.Errorf("canOpenStream is true for a job streamURL has no page for (%q)", streamURL(j))
	}
}

// An import uploads up to 500 MB and the server extracts up to 2 GB before it
// answers, so it gets its own long timeout on the shared transport — the API
// client's 30 s reported good imports as failed while the server finished
// them. Driven with an API client whose timeout is far shorter than the
// stand-in server's reply, so only importClient's timeout lets it through.
//
// Mutant: importFileCmd calling client.Do with the API client as-is, or
// importClient mutating the shared client's timeout.
func TestImportOutlastsTheAPIClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"title":"Imported"}`)
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	a := NewApp()
	a.SetWebPort(func() int { return port })
	api := a.apiClient()
	api.Timeout = 50 * time.Millisecond // stands in for the 30 s an import outlives

	zipPath := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(zipPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, ok := a.importFileCmd(zipPath)().(importResultMsg)
	if !ok || msg.Err != "" || msg.Title != "Imported" {
		t.Fatalf("an import slower than the API client's timeout = %#v, want success", msg)
	}
	if api.Timeout != 50*time.Millisecond {
		t.Errorf("the import changed the shared API client's timeout to %v", api.Timeout)
	}
}

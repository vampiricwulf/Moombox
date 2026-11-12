package tui

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A refused add keeps its dialog open with the input intact and shows the
// reason as the dialog's error; Enter submits again.
// Mutant: closing on any result (what it did) — the URL is thrown away.
func TestRefusedAddVideoKeepsTheDialogAndItsInput(t *testing.T) {
	for _, size := range [][2]int{{100, 40}, {30, 6}} {
		a := NewApp()
		a.width, a.height = size[0], size[1]
		a.addVideo.SetSize(size[0], size[1])
		a.addVideo.Open()
		a.addVideo.urlInput = "https://www.twitch.tv/nosuchchanel"
		if act, _ := a.addVideo.HandleKey(keyEnter); act != "submit" {
			t.Fatalf("setup: Enter gave %q, want submit", act)
		}
		id := a.addVideo.videoID

		a.Update(addVideoResultMsg{VideoID: id, Feedback: "Twitch channel not found", Err: true})

		if !a.addVideo.IsVisible() {
			t.Fatalf("%v: a refused add closed the dialog", size)
		}
		if a.addVideo.submitting {
			t.Errorf("%v: the dialog still says it is adding", size)
		}
		if a.addVideo.urlInput != "https://www.twitch.tv/nosuchchanel" {
			t.Errorf("%v: input = %q, want it intact", size, a.addVideo.urlInput)
		}
		if got := stripANSI(a.addVideo.View()); !strings.Contains(got, "Twitch channel not found") {
			t.Errorf("%v: the dialog does not show the refusal:\n%s", size, got)
		}
		if a.feedback.msg != "" {
			t.Errorf("%v: feedback = %q; the reason belongs in the dialog", size, a.feedback.msg)
		}
		if act, _ := a.addVideo.HandleKey(keyEnter); act != "submit" {
			t.Errorf("%v: Enter after a refusal gave %q, want a new submit", size, act)
		}
	}
}

// With the dialog gone (Esc during the request) the refusal goes to the
// feedback line, stated as an error: "Twitch channel not found" has no word
// the fallback colour scan reads, and was drawn in success green.
func TestRefusedAddVideoWithoutItsDialogIsAnErrorLine(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.addVideo.Open()
	a.addVideo.urlInput = "dQw4w9WgXcQ"
	a.addVideo.HandleKey(keyEnter)
	a.addVideo.HandleKey(keyEsc)

	a.Update(addVideoResultMsg{VideoID: "dQw4w9WgXcQ", Feedback: "Twitch channel not found", Err: true})
	if a.feedback.msg != "Twitch channel not found" || a.feedback.sev != severityError {
		t.Errorf("feedback = %q sev %v, want the reason as an error", a.feedback.msg, a.feedback.sev)
	}

	// A refusal for an earlier submit leaves a reopened dialog alone.
	a.addVideo.Open()
	a.addVideo.urlInput = "half-typed"
	a.Update(addVideoResultMsg{VideoID: "dQw4w9WgXcQ", Feedback: "nope", Err: true})
	if !a.addVideo.IsVisible() || a.addVideo.errorMsg != "" {
		t.Errorf("an earlier submit's refusal reached the reopened dialog (error %q)", a.addVideo.errorMsg)
	}
}

// What the command reports: a refusal is an Err, an existing job a Warn that
// still closes the dialog as before.
func TestAddVideoCmdStatesRefusalsAndDuplicates(t *testing.T) {
	status, body := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	a := NewApp()
	a.SetWebPort(func() int { return port })
	a.addVideo.platform = "youtube"

	status, body = 404, `{"error":"Twitch channel not found"}`
	msg := a.addVideoCmd("dQw4w9WgXcQ")().(addVideoResultMsg)
	if !msg.Err || msg.Feedback != "Twitch channel not found" {
		t.Errorf("404 = %+v, want an Err with the server's reason", msg)
	}

	status, body = 409, `{"error":"Job already exists"}`
	msg = a.addVideoCmd("dQw4w9WgXcQ")().(addVideoResultMsg)
	if msg.Err || !msg.Warn || msg.Feedback != "Job already exists" {
		t.Errorf("409 = %+v, want a Warn and no Err", msg)
	}
	a.width, a.height = 100, 40
	a.addVideo.Open()
	a.addVideo.urlInput = "dQw4w9WgXcQ"
	a.addVideo.HandleKey(keyEnter)
	a.Update(msg)
	if a.addVideo.IsVisible() || a.feedback.sev != severityWarning {
		t.Errorf("an existing job must close the dialog with a warning (visible %v, sev %v)", a.addVideo.IsVisible(), a.feedback.sev)
	}
}

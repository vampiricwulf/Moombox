package tui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// importAnsweredWith runs A Z's upload against a stand-in server that answers
// status and body, and returns the result message.
func importAnsweredWith(t *testing.T, a *App, status int, body string) importResultMsg {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	a.SetWebPort(func() int { return port })

	zipPath := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(zipPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, ok := a.importFileCmd(zipPath)().(importResultMsg)
	if !ok {
		t.Fatalf("importFileCmd answered %T", msg)
	}
	return msg
}

// W25-01: an import that re-adopted the identical files a deleted row left in
// imports/, or took " (2)" beside a different file, says so on the feedback
// line, whole — the note ends in the file names — in yellow for a rename or a
// chat left out (W25-04) and green for a re-adoption. A plain import keeps
// "Imported: <title>".
//
// Mutants: importFileCmd not decoding import.note (the plain line); Warn not
// set from import.renamed or from import.unpairedChats, or the update
// ignoring it (green for a warning); setFeedback in place of
// setWrappedFeedback (the note is cut to one row).
func TestImportOutcomeReachesTheFeedbackLine(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantSev    feedbackSeverity
		wantNote   string
	}{
		{"renamed", `{"title":"Stream","import":{"renamed":[{"from":"a.mp4","to":"a (2).mp4"}],"note":"imports/ already held a different \"a.mp4\"; imported as \"a (2).mp4\""}}`,
			severityWarning, `imported as "a (2).mp4"`},
		{"re-adopted", `{"title":"Stream","import":{"readopted":["a.mp4"],"note":"kept the identical copy already in imports/: a.mp4"}}`,
			severitySuccess, "kept the identical copy"},
		{"a chat left out", `{"title":"Stream","import":{"unpairedChats":["B.chat.json"],"note":"left out B.chat.json: a chat is imported only beside the video its name matches"}}`,
			severityWarning, "left out B.chat.json"},
		{"plain", `{"title":"Stream","import":{}}`, severityUnstated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp()
			a.width, a.height = 60, 40
			msg := importAnsweredWith(t, a, http.StatusCreated, tc.body)
			a.Update(msg)
			if !strings.HasPrefix(a.feedback.msg, "Imported: Stream") {
				t.Fatalf("feedback %q", a.feedback.msg)
			}
			if a.feedback.sev != tc.wantSev {
				t.Errorf("severity %v, want %v", a.feedback.sev, tc.wantSev)
			}
			if tc.wantNote == "" {
				if a.feedback.msg != "Imported: Stream" || a.feedback.wrap {
					t.Errorf("a plain import's line changed: %q (wrap %v)", a.feedback.msg, a.feedback.wrap)
				}
				return
			}
			if !strings.Contains(a.feedback.msg, tc.wantNote) || !a.feedback.wrap {
				t.Errorf("feedback %q (wrap %v), want the whole note %q", a.feedback.msg, a.feedback.wrap, tc.wantNote)
			}
		})
	}
}

// W25-04: a refused import — a zip holding more than one recording — puts the
// server's reason, which names the videos, in the dialog.
func TestImportRefusalReasonReachesTheDialog(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.importDlg.SetSize(100, 40)
	a.importDlg.visible, a.importDlg.step = true, importStepImporting
	reason := "the zip holds more than one recording: A.mp4, B.mp4 — import one recording per zip"
	a.Update(importAnsweredWith(t, a, http.StatusBadRequest, `{"error":"`+reason+`"}`))
	if a.importDlg.errorMsg != reason {
		t.Errorf("dialog error %q, want %q", a.importDlg.errorMsg, reason)
	}
}

// imports-5: after a refused import the metadata step's marked field takes
// typing again — Enter blurred the input to start the import, and the
// refusal must give the focus back, not leave a field marked "> " that
// ignores every key until Tab.
//
// Mutant: SetError setting the step without refocusing the active field.
func TestImportRefusalKeepsTheFieldTypeable(t *testing.T) {
	for _, focus := range []int{0, 1} {
		a := NewApp()
		a.width, a.height = 100, 40
		a.importDlg.SetSize(100, 40)
		a.importDlg.visible, a.importDlg.step = true, importStepMeta
		a.importDlg.filePath = "a.zip"
		a.importDlg.title, a.importDlg.channel = "Old", "Chan"
		a.importDlg.metaFocus = focus
		a.importDlg.syncToTextInput()

		a.Update(keyMsg("enter"))
		if a.importDlg.step != importStepImporting {
			t.Fatalf("focus %d: Enter left the step at %v", focus, a.importDlg.step)
		}
		a.Update(importAnsweredWith(t, a, http.StatusBadRequest, `{"error":"refused"}`))
		a.Update(keyMsg("x"))
		got := a.importDlg.title
		want := "Oldx"
		if focus == 1 {
			got, want = a.importDlg.channel, "Chanx"
		}
		if got != want {
			t.Errorf("focus %d: typed after a refusal, field = %q, want %q", focus, got, want)
		}
	}
}

// imports-3: an archive past the import's upload cap is refused before a
// byte is sent — the TUI's request is chunked, so the server learns its size
// only by spooling 500 MB of it — naming both sizes; a small one is sent.
//
// Mutant: importFileCmd opening and posting the file without the size check.
func TestImportRefusesAnArchiveOverTheCapBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		size int64
		sent bool
	}{
		{constants.MaxImportUploadBytes + 1, false},
		{1 << 10, true},
	} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"x"}`)
		}))
		a := NewApp()
		_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
		port, _ := strconv.Atoi(portStr)
		a.SetWebPort(func() int { return port })
		zipPath := filepath.Join(t.TempDir(), "big.zip")
		f, err := os.Create(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(tc.size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		msg, _ := a.importFileCmd(zipPath)().(importResultMsg)
		srv.Close()
		if got := hits.Load() > 0; got != tc.sent {
			t.Errorf("size %d: sent = %v, want %v (%+v)", tc.size, got, tc.sent, msg)
		}
		if !tc.sent && (!strings.Contains(msg.Err, "big.zip") || !strings.Contains(msg.Err, utils.FormatFileSize(constants.MaxImportUploadBytes))) {
			t.Errorf("size %d: refusal %q does not name the file and the cap", tc.size, msg.Err)
		}
	}
}

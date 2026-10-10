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
	"testing"
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
// line, whole — the note ends in the file names — in yellow for a rename and
// green for a re-adoption. A plain import keeps "Imported: <title>".
//
// Mutants: importFileCmd not decoding import.note (the plain line);
// Renamed not set from import.renamed, or the update ignoring it (green for
// a rename); setFeedback in place of setWrappedFeedback (the note is cut to
// one row).
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
	a.importDlg.visible, a.importDlg.step = true, 2
	reason := "the zip holds more than one recording: A.mp4, B.mp4 — import one recording per zip"
	a.Update(importAnsweredWith(t, a, http.StatusBadRequest, `{"error":"`+reason+`"}`))
	if a.importDlg.errorMsg != reason {
		t.Errorf("dialog error %q, want %q", a.importDlg.errorMsg, reason)
	}
}

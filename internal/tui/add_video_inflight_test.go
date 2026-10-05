package tui

import "testing"

// TestAddVideoSubmitsOnce: Enter returned "submit" every time, with nothing
// on screen while POST /api/jobs (a live metadata fetch, seconds long) ran, so
// a second Enter posted again and its 409 overwrote "Added to queue" with
// "Job already exists". The dialog now holds an in-flight state: no second
// submit, and Esc closes it.
//
// Mutant: dropping startSubmit's flag — the second Enter submits again.
func TestAddVideoSubmitsOnce(t *testing.T) {
	m := NewAddVideoModel()
	m.Open()
	m.urlInput = "dQw4w9WgXcQ"
	if a, _ := m.HandleKey(keyEnter); a != "submit" {
		t.Fatalf("first Enter = %q, want submit", a)
	}
	if a, _ := m.HandleKey(keyEnter); a != "" {
		t.Errorf("second Enter while the first is in flight = %q, want nothing", a)
	}
	m.HandleKey(keyEsc)
	if m.IsVisible() {
		t.Error("Esc during the submit did not close the dialog")
	}
}

// TestAddVideoResultClosesOnlyItsOwnDialog: addVideoResultMsg closed
// whichever Add Video dialog was open — submit, Esc, A A, start typing, and
// the first video's answer closed the second dialog mid-word. It now closes
// only the dialog waiting on that video, and the feedback line always gets
// the answer.
//
// Mutant: closing unconditionally again — the reopened dialog vanishes.
func TestAddVideoResultClosesOnlyItsOwnDialog(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.addVideo.Open()
	a.addVideo.urlInput = "dQw4w9WgXcQ"
	a.addVideo.HandleKey(keyEnter)
	a.addVideo.HandleKey(keyEsc)
	a.addVideo.Open()
	a.addVideo.urlInput = "half-typed"

	a.Update(addVideoResultMsg{VideoID: "dQw4w9WgXcQ", Feedback: "Added to queue"})
	if !a.addVideo.IsVisible() {
		t.Error("the first submission's result closed the second dialog")
	}
	if a.feedback.msg != "Added to queue" {
		t.Errorf("feedback = %q, want the first submission's answer", a.feedback.msg)
	}

	a.addVideo.urlInput = "jNQXAC9IVRw"
	a.addVideo.HandleKey(keyEnter)
	a.Update(addVideoResultMsg{VideoID: "jNQXAC9IVRw", Feedback: "Added to queue"})
	if a.addVideo.IsVisible() {
		t.Error("the dialog's own result did not close it")
	}
}

// TestAddVideoDropsAFormatsResultItStoppedWaitingFor: advanced on, fetch A,
// Esc, fetch B — and A's late answer filled the table and the Confirm step's
// Title with A's formats while the dialog's video was B, so the itag picked
// was sent for B. A result is applied only to the fetch the dialog is waiting
// on.
//
// Mutant: dropping the AwaitingFormats gate — A's formats land on B.
func TestAddVideoDropsAFormatsResultItStoppedWaitingFor(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.addVideo.Open()
	a.addVideo.HandleKey(keyTab)
	a.addVideo.urlInput = "AAAAAAAAAAA"
	if act, _ := a.addVideo.HandleKey(keyEnter); act != "fetch_formats" {
		t.Fatalf("setup: %q", act)
	}
	a.addVideo.HandleKey(keyEsc)
	a.addVideo.urlInput = "BBBBBBBBBBB"
	if act, _ := a.addVideo.HandleKey(keyEnter); act != "fetch_formats" {
		t.Fatalf("setup: %q", act)
	}

	a.Update(fetchFormatsResultMsg{VideoID: "AAAAAAAAAAA", Formats: &FormatsData{VideoID: "AAAAAAAAAAA", Title: "video A"}})
	if a.addVideo.formats != nil {
		t.Fatalf("A's formats were applied while the dialog waits on B: %+v", a.addVideo.formats)
	}
	a.Update(fetchFormatsResultMsg{VideoID: "BBBBBBBBBBB", Formats: &FormatsData{VideoID: "BBBBBBBBBBB", Title: "video B"}})
	if a.addVideo.formats == nil || a.addVideo.formats.Title != "video B" || a.addVideo.loading {
		t.Errorf("B's own result was not applied: formats=%+v loading=%v", a.addVideo.formats, a.addVideo.loading)
	}
}

// TestAddVideoAutoAdvanceHonoursEsc: a failed fetch arms a 2 s jump to
// Confirm, and it fired through an Esc pressed inside that window — the
// dialog went back to Confirm for the abandoned video, one Enter from
// submitting it.
//
// Mutant: gating the advance on errorMsg alone again — it fires after Esc.
func TestAddVideoAutoAdvanceHonoursEsc(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.addVideo.Open()
	a.addVideo.HandleKey(keyTab)
	a.addVideo.urlInput = "dQw4w9WgXcQ"
	a.addVideo.HandleKey(keyEnter)
	a.Update(fetchFormatsResultMsg{VideoID: "dQw4w9WgXcQ", Err: "Failed to fetch formats. Proceeding with auto selection."})
	a.addVideo.HandleKey(keyEsc)

	a.Update(fetchFormatsAutoAdvanceMsg{VideoID: "dQw4w9WgXcQ"})
	if a.addVideo.step != AddStepURL {
		t.Errorf("auto-advance moved the dialog to step %v after Esc", a.addVideo.step)
	}

	// Without the Esc it still advances.
	a.addVideo.HandleKey(keyEnter)
	a.Update(fetchFormatsResultMsg{VideoID: "dQw4w9WgXcQ", Err: "Failed to fetch formats. Proceeding with auto selection."})
	a.Update(fetchFormatsAutoAdvanceMsg{VideoID: "dQw4w9WgXcQ"})
	if a.addVideo.step != AddStepConfirm {
		t.Errorf("a failed fetch no longer auto-advances: step %v", a.addVideo.step)
	}
}

package tui

import (
	"strings"
	"testing"
)

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

// TestAddVideoRefusesTwitchClipsAndListings: a channel's clip list and a
// channel's collection list are refused in the dialog with the add paths'
// shared message, instead of a listing being read as the channel and
// submitted as its live-wait job; a clip — its own job since owner decision
// D26 — is submitted as "tw_clip_<slug>".
//
// Mutants: dropping the "twitch_clips" case — the clip list submits; dropping
// the collections refusal — ?filter=collections submits as a video list; the
// clip refusal restored — the clip does not submit.
func TestAddVideoRefusesTwitchClipsAndListings(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.twitch.tv/spamfish/clips":                     "Twitch clip lists are not supported yet.",
		"https://www.twitch.tv/spamfish/videos?filter=collections": "Twitch collection lists are not supported yet.",
	} {
		m := NewAddVideoModel()
		m.Open()
		m.urlInput = input
		if a, _ := m.HandleKey(keyEnter); a != "" {
			t.Errorf("%s: Enter = %q, want a refusal", input, a)
		}
		if !strings.HasPrefix(m.errorMsg, want) {
			t.Errorf("%s: errorMsg = %q, want it to start %q", input, m.errorMsg, want)
		}
	}
	m := NewAddVideoModel()
	m.Open()
	m.urlInput = "https://www.twitch.tv/clip/FaintLightGullWholeWheat"
	if a, id := m.HandleKey(keyEnter); a != "submit" || id != "tw_clip_FaintLightGullWholeWheat" || m.errorMsg != "" {
		t.Errorf("clip: Enter = (%q, %q), errorMsg %q; want it submitted as tw_clip_FaintLightGullWholeWheat", a, id, m.errorMsg)
	}
}

// TestAddVideoSubmitsTwitchListings is owner decision D18: a channel's video
// list and a collection submit, with the input as pasted, for the server to
// queue one VOD job per video.
//
// Mutant: the "twitch_listing" case refusing again — Enter is no submit.
func TestAddVideoSubmitsTwitchListings(t *testing.T) {
	for _, input := range []string{
		"https://www.twitch.tv/spamfish/videos?filter=archives",
		"https://www.twitch.tv/collections/o9zZer3IQBhTJw",
	} {
		m := NewAddVideoModel()
		m.Open()
		m.urlInput = input
		if a, id := m.HandleKey(keyEnter); a != "submit" || id != input || m.GetPlatform() != "twitch_listing" {
			t.Errorf("%s: Enter = (%q, %q), platform %q; want a twitch_listing submit of the input", input, a, id, m.GetPlatform())
		}
	}
}

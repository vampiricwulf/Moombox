package tui

import (
	"strings"
	"testing"
)

func TestReleaseNotesOverlayInitiallyClosed(t *testing.T) {
	o := newReleaseNotesOverlay()
	if o.isOpen() {
		t.Error("new overlay should not be open")
	}
}

func TestReleaseNotesOverlayOpenStores(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "## Features\n- thing", 80, 24)
	if !o.isOpen() {
		t.Error("overlay should be open after open()")
	}
	if o.tag != "v2.7.0" {
		t.Errorf("tag: got %q, want v2.7.0", o.tag)
	}
}

func TestReleaseNotesOverlayCloseClears(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "x", 80, 24)
	o.close()
	if o.isOpen() {
		t.Error("overlay should be closed after close()")
	}
}

func TestReleaseNotesOverlayRenderRendersHeading(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "## Features\n- thing\n\n## Bug Fixes\n- another", 80, 24)
	view := o.View()
	if view == "" {
		t.Fatal("View returned empty string")
	}
	// Glamour replaces "## Features" with stylized text — we don't assert
	// on exact ANSI, just that the original raw markdown isn't passed
	// through unchanged.
	if strings.Contains(view, "## Features") {
		t.Errorf("View still contains raw markdown ##: %q", view)
	}
	// The tag should appear in the title bar.
	if !strings.Contains(view, "v2.7.0") {
		t.Errorf("View missing tag in title: %q", view)
	}
}

func TestReleaseNotesOverlayRenderEmptyNotes(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "", 80, 24)
	view := o.View()
	if !strings.Contains(view, "No release notes") {
		t.Errorf("expected 'No release notes' fallback, got: %q", view)
	}
}

package routes

import (
	"strings"
	"testing"
)

// TestParityButtonsArePinned pins the two Arc F web buttons the way the
// logout icon is pinned (logout_wiring_test.go): the markup exists, the
// handler is wired, and the server route each one POSTs to is the one that
// exists. A mutant that drops the button or its listener passes node
// 100 %; this is what fails.
func TestParityButtonsArePinned(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	settingsJS := readEmbeddedModule(t, "public/modules/settings.js")
	// The details dialog (and its Copy stream URL button) lives in
	// job-details.js since Arc I extracted JobDetailsController from app.js.
	detailsJS := readEmbeddedModule(t, "public/modules/job-details.js")

	if !strings.Contains(html, `id="rescan-feeds-btn"`) {
		t.Error("index.html lacks the rescan-feeds-btn")
	}
	if !strings.Contains(settingsJS, `document.getElementById("rescan-feeds-btn")`) {
		t.Error("settings.js does not wire rescan-feeds-btn")
	}
	if body := jsMethodBody(t, settingsJS, "rescanFeedHistory"); !strings.Contains(body, `fetch("/api/backfill/rescan", { method: "POST" })`) {
		t.Error("rescanFeedHistory does not POST /api/backfill/rescan")
	}
	// Tightened beyond a bare "import {" + "streamUrl" substring check (which
	// would also pass if streamUrl were merely used, never imported): find
	// the utils.js import line specifically and require streamUrl inside it.
	utilsImportLine := ""
	for line := range strings.SplitSeq(detailsJS, "\n") {
		if strings.HasPrefix(line, "import {") && strings.Contains(line, `from "./utils.js"`) {
			utilsImportLine = line
			break
		}
	}
	if utilsImportLine == "" {
		t.Fatal("job-details.js has no utils.js import line to check")
	}
	if !strings.Contains(utilsImportLine, "streamUrl") {
		t.Error("job-details.js does not import streamUrl from ./utils.js")
	}
	if !strings.Contains(detailsJS, `data-copy="${this.app.escapeHtml(streamUrl(job))}"`) {
		t.Error("the details dialog has no Copy stream URL button")
	}
}

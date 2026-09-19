package routes

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	webassets "github.com/vampiricwulf/Moombox/web"
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

// TestNoAlertInTheWebUI — every other failure in the dashboard surfaces as a
// toast; one alert survived in the View Release Notes handler, where it blocks
// the whole tab and looks like a browser error rather than a Moombox one.
//
// The file list is the embedded tree itself rather than a hand-written roster,
// so a module added later is covered the day it lands. Full-line comments are
// skipped: the replacement comment at the fixed site names the call it
// replaced, and a test that reads its own justification as the defect is a
// test nobody can write a comment next to.
//
// MUTANT: restore the alert( call anywhere under web/public — this finds it and
// names the file and line.
func TestNoAlertInTheWebUI(t *testing.T) {
	// A bare alert(...) or window.alert(...) call, never the sl-alert element
	// name (the `-` is excluded from the leading class) and never a property
	// access like foo.alert(.
	bareAlert := regexp.MustCompile(`(^|[^\w$.\-])alert\s*\(`)
	windowAlert := regexp.MustCompile(`\bwindow\.alert\s*\(`)

	var scanned int
	err := fs.WalkDir(webassets.PublicFS, "public", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(p, ".js") && !strings.HasSuffix(p, ".html")) {
			return nil
		}
		scanned++
		for i, line := range strings.Split(readEmbeddedModule(t, p), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "//"), strings.HasPrefix(trimmed, "/*"),
				strings.HasPrefix(trimmed, "*"):
				continue
			}
			if bareAlert.MatchString(line) || windowAlert.MatchString(line) {
				t.Errorf("%s:%d uses alert(): %s — every other failure toasts", p, i+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the embedded public tree: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("only %d embedded .js/.html files scanned — the walk is not reaching web/public", scanned)
	}
}

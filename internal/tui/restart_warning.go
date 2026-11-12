package tui

import "fmt"

// The warning every restart prompt gives while downloads are active: the
// update restart (R U, the release notes' U), R P and the Settings restart
// prompt. A restart is the same process restart whichever of them asks, so
// they share one count (App.activeDownloadCount: jobs Downloading, Live or
// Muxing) and one wording. The dashboard's copies are activeDownloadCount and
// activeDownloadsRestartWarning in web/public/modules/utils.js, and the words
// here are the same as theirs.

// activeRestartClause says that n downloads are active and that restart
// ("restart", "update restart") interrupts them. "" when n is 0. It is the
// short form, for a confirm line.
func activeRestartClause(n int, restart string) string {
	if n <= 0 {
		return ""
	}
	noun := "downloads are"
	if n == 1 {
		noun = "download is"
	}
	return fmt.Sprintf("%d %s active — the %s interrupts them", n, noun, restart)
}

// joinRestartNotes puts a second note (a Resume or Reinitialize still
// running: App.jobVerbRunningNote) after the active-downloads clause. Either
// may be "".
func joinRestartNotes(clause, note string) string {
	if clause != "" && note != "" {
		return clause + "; " + note
	}
	return clause + note
}

// activeRestartWarning is the whole sentence, as the dashboard's prompts
// show it, for a prompt with room. "" when n is 0.
func activeRestartWarning(n int, restart string) string {
	clause := activeRestartClause(n, restart)
	if clause == "" {
		return ""
	}
	return clause + ", and live segments during the ~30s gap may be lost (Twitch especially)."
}

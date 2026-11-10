package tui

import tea "charm.land/bubbletea/v2"

// overlayEntry is one full-screen overlay as the App routes to it. Each
// overlay keeps its own model and visibility flag; the registry holds only
// how the App reaches it, and — by its order — which overlay wins.
type overlayEntry struct {
	name string
	// title is the overlay's name as the operator reads it, on the notice
	// that covers it in a terminal smaller than it needs.
	title string
	// bare is the overlay's bare form, drawn below the normal layout
	// (overlay_bare.go): the smallest terminal it opens in, and what its
	// current state needs when that is more. nil: it has none and needs the
	// normal layout (60x20). The overlay's own View draws the bare form.
	// Every overlay states its answer in overlayCases
	// (overlay_order_test.go).
	bare    *overlayBare
	visible func(*App) bool
	// view renders the overlay; the App wraps it in viewWithMode.
	view func(*App) string
	// components feeds a non-key message (and every key, after key) to the
	// overlay's embedded Charm components. nil: the overlay owns none, but
	// it still blocks the fallthrough, so keys pressed over it never scroll
	// the hidden panel viewports underneath.
	components func(*App, tea.Msg) tea.Cmd
	// key handles a key press while the overlay is on top. It owns every
	// key: nothing reaches the chord system underneath.
	key func(*App, tea.KeyPressMsg, string) (tea.Model, tea.Cmd)
	// mouse handles a mouse event while the overlay is on top. nil: the
	// overlay swallows the mouse. Every overlay with something to scroll
	// has one (owner ruling R-C2): a text overlay scrolls three rows a
	// wheel notch, a list or table overlay moves its cursor one row. The
	// census (overlayWheel, overlay_order_test.go) states the answer for
	// every overlay.
	mouse func(*App, tea.MouseMsg) (tea.Model, tea.Cmd)
	// liveProgress reports whether the overlay is, right now, drawing progress
	// the progress tick delivers, so the tick must stay at the fast class
	// while it is on top (wantsFastProgress). nil: the overlay draws none,
	// and the tick idles under it. Only the trim dialog sets it; the census
	// in overlay_order_test.go states the answer for every overlay.
	liveProgress func(*App) bool
	// heldEsc reports what Esc does to the overlay, right now, while it is
	// kept under the notice in a terminal smaller than it needs
	// (overlay_held.go). nil: its Esc closes it from every state. The census
	// is TestHeldEscCensus (app_compact_test.go).
	heldEsc func(*App) heldEscKind
}

// overlayRegistry is THE overlay order (K23): handleKey, handleMouse, View,
// routeComponentMsg and hasActiveOverlay all take the first visible entry,
// so a key, a click and the screen always go to the same overlay. Overlays
// are exclusive in practice (every opener runs from the base state or
// closes the overlay it came from); the order decides only what would win
// if two were ever open. Ctrl+C is checked in handleKey ahead of all of
// them. overlay_order_test.go pins this order and censuses every entry.
//
// Built in init so the handlers it names may themselves consult the
// registry (hasActiveOverlay) without an initialisation cycle.
var overlayRegistry []overlayEntry

func init() {
	overlayRegistry = []overlayEntry{
		{
			name:       "settings",
			title:      "Settings",
			heldEsc:    func(a *App) heldEscKind { return a.settings.heldEsc() },
			visible:    func(a *App) bool { return a.settings.IsVisible() },
			view:       func(a *App) string { return a.settings.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.settings.UpdateComponents(msg) },
			key:        (*App).keySettings,
			mouse:      (*App).mouseSettings,
		},
		{
			name:       "help",
			title:      "Help",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.help.IsVisible() },
			view:       func(a *App) string { return a.help.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.help.UpdateViewport(msg) },
			key:        (*App).keyHelp,
			mouse:      (*App).mouseHelp,
		},
		{
			name:       "release notes",
			title:      "Release Notes",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.releaseNotesPopup != nil && a.releaseNotesPopup.isOpen() },
			view:       func(a *App) string { return a.releaseNotesPopup.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.releaseNotesPopup.Update(msg) },
			key:        (*App).keyReleaseNotes,
			mouse:      (*App).mouseReleaseNotes,
		},
		{
			name:       "ffmpeg check",
			title:      "FFmpeg Check",
			heldEsc:    func(a *App) heldEscKind { return a.ffmpegCheck.heldEsc() },
			visible:    func(a *App) bool { return a.ffmpegCheck.IsVisible() },
			view:       func(a *App) string { return a.ffmpegCheck.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.ffmpegCheck.UpdateComponents(msg) },
			key:        (*App).keyFFmpegCheck,
			mouse:      (*App).mouseFFmpegCheck,
		},
		{
			name:       "setup wizard",
			title:      "Setup",
			heldEsc:    func(a *App) heldEscKind { return a.setupWiz.heldEsc() },
			visible:    func(a *App) bool { return a.setupWiz.IsVisible() },
			view:       func(a *App) string { return a.setupWiz.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.setupWiz.UpdateComponents(msg) },
			key:        (*App).keySetupWizard,
			mouse:      (*App).mouseSetupWizard,
		},
		{
			name:    "action menu",
			title:   "Action Menu",
			heldEsc: func(a *App) heldEscKind { return a.actionMenu.heldEsc() },
			visible: func(a *App) bool { return a.actionMenu.IsVisible() },
			view:    func(a *App) string { return a.actionMenu.View() },
			key:     (*App).keyActionMenu,
			mouse:   (*App).mouseActionMenu,
		},
		{
			name:       "import",
			title:      "Import Archive",
			heldEsc:    func(a *App) heldEscKind { return a.importDlg.heldEsc() },
			visible:    func(a *App) bool { return a.importDlg.IsVisible() },
			view:       func(a *App) string { return a.importDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.importDlg.UpdateComponents(msg) },
			key:        (*App).keyImport,
			mouse:      (*App).mouseImport,
		},
		{
			name:       "cookie import",
			title:      "Import Cookie File",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.cookieImportDlg.IsVisible() },
			view:       func(a *App) string { return a.cookieImportDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.cookieImportDlg.UpdateComponents(msg) },
			key:        (*App).keyCookieImport,
		},
		{
			name:       "chapter import",
			title:      "Import Chapters",
			heldEsc:    func(a *App) heldEscKind { return a.chapterDlg.heldEsc() },
			visible:    func(a *App) bool { return a.chapterDlg.IsVisible() },
			view:       func(a *App) string { return a.chapterDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.chapterDlg.UpdateComponents(msg) },
			key:        (*App).keyChapterImport,
			mouse:      (*App).mouseChapterImport,
		},
		{
			name:       "add video",
			title:      "Add Video",
			heldEsc:    func(a *App) heldEscKind { return a.addVideo.heldEsc() },
			visible:    func(a *App) bool { return a.addVideo.IsVisible() },
			view:       func(a *App) string { return a.addVideo.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.addVideo.UpdateComponents(msg) },
			key:        (*App).keyAddVideo,
			mouse:      (*App).mouseAddVideo,
		},
		{
			name:         "trim",
			title:        "Trim Video",
			heldEsc:      func(a *App) heldEscKind { return a.trimDlg.heldEsc() },
			visible:      func(a *App) bool { return a.trimDlg.IsVisible() },
			view:         func(a *App) string { return a.trimDlg.View() },
			components:   func(a *App, msg tea.Msg) tea.Cmd { return a.trimDlg.UpdateComponents(msg) },
			key:          (*App).keyTrim,
			mouse:        (*App).mouseTrim,
			liveProgress: func(a *App) bool { return a.trimInProgress },
		},
		{
			name:       "files",
			title:      "Orphaned Items",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.filesDlg.IsVisible() },
			view:       func(a *App) string { return a.filesDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.filesDlg.UpdateComponents(msg) },
			key:        (*App).keyFiles,
			mouse:      (*App).mouseFiles,
		},
		{
			name:       "client tokens",
			title:      "Client Tokens",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.clientTokensDlg.IsVisible() },
			view:       func(a *App) string { return a.clientTokensDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.clientTokensDlg.UpdateComponents(msg) },
			key:        (*App).keyClientTokens,
			mouse:      (*App).mouseClientTokens,
		},
		{
			name:       "ytdlp plugin",
			title:      "yt-dlp Plugin",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.ytdlpDlg.IsVisible() },
			view:       func(a *App) string { return a.ytdlpDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.ytdlpDlg.UpdateComponents(msg) },
			key:        (*App).keyYtdlp,
		},
		{
			name:       "stats",
			title:      "Statistics",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.statsDlg.IsVisible() },
			view:       func(a *App) string { return a.statsDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.statsDlg.UpdateComponents(msg) },
			key:        (*App).keyStats,
			mouse:      (*App).mouseStats,
		},
		{
			name:       "job log",
			title:      "Job Log",
			bare:       &overlayBare{openW: compactMinWidth, openH: compactMinHeight},
			visible:    func(a *App) bool { return a.jobLog.IsVisible() },
			view:       func(a *App) string { return a.jobLog.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.jobLog.UpdateComponents(msg) },
			key:        (*App).keyJobLog,
			mouse:      (*App).mouseJobLog,
		},
	}
}

// activeOverlay returns the overlay on top — the first visible registry
// entry — or nil when only the panels are showing.
func (a *App) activeOverlay() *overlayEntry {
	for i := range overlayRegistry {
		if overlayRegistry[i].visible(a) {
			return &overlayRegistry[i]
		}
	}
	return nil
}

// hasActiveOverlay reports whether any overlay is open.
func (a *App) hasActiveOverlay() bool {
	return a.activeOverlay() != nil
}

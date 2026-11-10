package tui

import tea "charm.land/bubbletea/v2"

// overlayEntry is one full-screen overlay as the App routes to it. Each
// overlay keeps its own model and visibility flag; the registry holds only
// how the App reaches it, and — by its order — which overlay wins.
type overlayEntry struct {
	name    string
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
	// overlay swallows the mouse.
	mouse func(*App, tea.MouseMsg) (tea.Model, tea.Cmd)
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
			visible:    func(a *App) bool { return a.settings.IsVisible() },
			view:       func(a *App) string { return a.settings.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.settings.UpdateComponents(msg) },
			key:        (*App).keySettings,
			mouse:      (*App).mouseSettings,
		},
		{
			name:       "help",
			visible:    func(a *App) bool { return a.help.IsVisible() },
			view:       func(a *App) string { return a.help.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.help.UpdateViewport(msg) },
			key:        (*App).keyHelp,
		},
		{
			name:       "release notes",
			visible:    func(a *App) bool { return a.releaseNotesPopup != nil && a.releaseNotesPopup.isOpen() },
			view:       func(a *App) string { return a.releaseNotesPopup.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.releaseNotesPopup.Update(msg) },
			key:        (*App).keyReleaseNotes,
		},
		{
			name:       "ffmpeg check",
			visible:    func(a *App) bool { return a.ffmpegCheck.IsVisible() },
			view:       func(a *App) string { return a.ffmpegCheck.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.ffmpegCheck.UpdateComponents(msg) },
			key:        (*App).keyFFmpegCheck,
		},
		{
			name:       "setup wizard",
			visible:    func(a *App) bool { return a.setupWiz.IsVisible() },
			view:       func(a *App) string { return a.setupWiz.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.setupWiz.UpdateComponents(msg) },
			key:        (*App).keySetupWizard,
		},
		{
			name:    "action menu",
			visible: func(a *App) bool { return a.actionMenu.IsVisible() },
			view:    func(a *App) string { return a.actionMenu.View() },
			key:     (*App).keyActionMenu,
			mouse:   (*App).mouseActionMenu,
		},
		{
			name:       "import",
			visible:    func(a *App) bool { return a.importDlg.IsVisible() },
			view:       func(a *App) string { return a.importDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.importDlg.UpdateComponents(msg) },
			key:        (*App).keyImport,
		},
		{
			name:       "cookie import",
			visible:    func(a *App) bool { return a.cookieImportDlg.IsVisible() },
			view:       func(a *App) string { return a.cookieImportDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.cookieImportDlg.UpdateComponents(msg) },
			key:        (*App).keyCookieImport,
		},
		{
			name:       "chapter import",
			visible:    func(a *App) bool { return a.chapterDlg.IsVisible() },
			view:       func(a *App) string { return a.chapterDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.chapterDlg.UpdateComponents(msg) },
			key:        (*App).keyChapterImport,
		},
		{
			name:       "add video",
			visible:    func(a *App) bool { return a.addVideo.IsVisible() },
			view:       func(a *App) string { return a.addVideo.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.addVideo.UpdateComponents(msg) },
			key:        (*App).keyAddVideo,
		},
		{
			name:       "trim",
			visible:    func(a *App) bool { return a.trimDlg.IsVisible() },
			view:       func(a *App) string { return a.trimDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.trimDlg.UpdateComponents(msg) },
			key:        (*App).keyTrim,
		},
		{
			name:       "files",
			visible:    func(a *App) bool { return a.filesDlg.IsVisible() },
			view:       func(a *App) string { return a.filesDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.filesDlg.UpdateComponents(msg) },
			key:        (*App).keyFiles,
		},
		{
			name:       "client tokens",
			visible:    func(a *App) bool { return a.clientTokensDlg.IsVisible() },
			view:       func(a *App) string { return a.clientTokensDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.clientTokensDlg.UpdateComponents(msg) },
			key:        (*App).keyClientTokens,
		},
		{
			name:       "ytdlp plugin",
			visible:    func(a *App) bool { return a.ytdlpDlg.IsVisible() },
			view:       func(a *App) string { return a.ytdlpDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.ytdlpDlg.UpdateComponents(msg) },
			key:        (*App).keyYtdlp,
		},
		{
			name:       "stats",
			visible:    func(a *App) bool { return a.statsDlg.IsVisible() },
			view:       func(a *App) string { return a.statsDlg.View() },
			components: func(a *App, msg tea.Msg) tea.Cmd { return a.statsDlg.UpdateComponents(msg) },
			key:        (*App).keyStats,
		},
		{
			name:       "job log",
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

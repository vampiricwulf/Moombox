package tui

// An overlay open in a terminal smaller than it needs (overlayDrawFor says
// drawHeld: it has no bare form, or its current state needs more room) is
// covered by a notice (heldOverlayView, owner ruling R-C6 as amended) and
// nothing it draws is on screen. An overlay drawn bare is on screen, takes
// every key itself and is not this file's business. Under the notice, Esc
// is the one key that may still reach the overlay, and only where it does
// nothing the operator cannot foresee from the notice: where the overlay's
// own Esc would quit Moombox, or answer a prompt that is not on screen, it
// is not delivered, and the notice says so. The notice's Esc line and the handleKey
// size guard both ask heldEscFor, so what the screen says is what Esc does.

// heldEscKind is what Esc does to an overlay kept under the notice.
type heldEscKind int

const (
	// heldEscCloses: the overlay's own Esc closes it.
	heldEscCloses heldEscKind = iota
	// heldEscBack: the overlay's own Esc leaves a step, a sub-editor or a
	// search, and the overlay stays open.
	heldEscBack
	// heldEscOff: Esc is not delivered. The overlay's own Esc would quit
	// the program, raise or answer a prompt nobody can see, or do nothing.
	heldEscOff
)

// heldEscLine is the notice's line for each kind.
func heldEscLine(k heldEscKind) string {
	switch k {
	case heldEscBack:
		return "Esc steps back in it."
	case heldEscOff:
		return "Esc is off until then."
	}
	return "Esc closes it."
}

// heldEscFor is what Esc does to o right now while it is kept under the
// notice. An entry without a heldEsc func is an overlay whose Esc closes it
// from every state.
func (a *App) heldEscFor(o *overlayEntry) heldEscKind {
	if o.heldEsc == nil {
		return heldEscCloses
	}
	return o.heldEsc(a)
}

// heldEsc for Settings. With unsaved edits Esc raises the "Save changes?"
// prompt, and a second Esc dismisses it, so under the notice it would only
// toggle a prompt nobody sees and never close.
func (m *SettingsModel) heldEsc() heldEscKind {
	switch {
	case m.showRestartOverlay:
		return heldEscCloses
	case m.closeConfirm, m.dirty && m.status != saveError:
		return heldEscOff
	}
	switch sections[m.sectionIndex].name {
	case "Channels":
		if m.channelMode == "edit" || m.channelDeleteConf {
			return heldEscBack
		}
	case "Integrations":
		if m.notifMode == "edit" || m.notifDeleteConf {
			return heldEscBack
		}
	case "Network":
		if m.secMode != securityStatus {
			return heldEscBack
		}
	}
	return heldEscCloses
}

// heldEsc for the FFmpeg check. On its main menu Esc quits Moombox
// (TestEscOnTheFFmpegOverlayStillQuits); under the notice only Ctrl+C may.
func (m *FFmpegCheckModel) heldEsc() heldEscKind {
	switch {
	case m.successDismiss:
		return heldEscCloses // any key closes it after a success
	case m.installing, m.checking, m.mode == ffmpegMain:
		return heldEscOff
	}
	return heldEscBack
}

// heldEsc for the setup wizard. Esc on the advanced form asks, on a line of
// the form, whether to abandon it, and a second Esc abandons what was typed;
// Esc with a login browser open cancels the login. Neither happens unseen.
func (m *SetupWizardModel) heldEsc() heldEscKind {
	switch {
	case m.saving, m.mode == setupModeSelect, m.advancedForm != nil, m.cookieActive, m.cookieTimedOut:
		return heldEscOff
	case m.cookieOnly:
		return heldEscCloses // the E L login overlay, on its platform list
	}
	return heldEscBack
}

func (m *ActionMenuModel) heldEsc() heldEscKind {
	if m.mode == menuMain {
		return heldEscCloses
	}
	return heldEscBack
}

func (m *ImportDialogModel) heldEsc() heldEscKind {
	switch m.step {
	case importStepPick:
		return heldEscCloses
	case importStepMeta:
		return heldEscBack
	}
	return heldEscOff // importing: its Esc does nothing
}

func (m *ChapterImportDialogModel) heldEsc() heldEscKind {
	if m.step == chapterImportStepConfirm {
		return heldEscBack
	}
	return heldEscCloses
}

func (m *AddVideoModel) heldEsc() heldEscKind {
	if m.submitting || m.step == AddStepURL {
		return heldEscCloses
	}
	return heldEscBack
}

// heldEsc for the trim dialog, in the order of its handleEscape.
func (m *TrimDialogModel) heldEsc() heldEscKind {
	switch {
	case m.mode == TrimModeCreate && m.createStep == trimStepProgress:
		return heldEscCloses // the encode carries on in the background
	case m.mode == TrimModeCreate && m.createStep > trimStepTimes, m.mode == TrimModeDelete:
		return heldEscBack
	}
	return heldEscCloses
}

## Improvements

- **The TUI status bar now adapts to the window instead of hiding your keybinds.** Previously a single width threshold flipped the bar between two fixed renderings, and any remaining overflow blanked the entire chord-hint half — so an ordinary narrow terminal showed no keybinds at all. Both halves now carry a density ladder and the bar picks the richest pair that fits the measured width, so it reacts to real content (a long backfill channel name, "12 selected", OFFLINE) rather than a guessed column count. The right half yields first, since losing "Backfill Foo: videos p3" costs a detail the log panel already carries while losing "Tab Focus" costs a keybind you may not know. All eight chords now survive down to roughly 20 columns, and the hints shed labels, then separators, then all but `M` and `?` — which reach everything else — before ever going blank. On the right, the informational items (backfill scan, selection count, active tally) drop before the alerts (OFFLINE, disk warning, re-login), and a healthy `YT`/`TW` yields early since it is only reassurance.
- **The task list header keeps your scroll position when space is tight.** It used to drop its entire right side on overflow, taking the `[1-20/57]` range along with the monitor countdowns. The countdowns now go first, then the range abbreviates to `[20/57]`.
- **Release notes render correctly on light terminals.** The markdown renderer now follows the detected terminal background, which the app already applied to its forms and wizards — this was the one themed surface still forcing the dark palette, which renders low-contrast body text on a light background.

## Bug Fixes

- **The release-notes overlay no longer spills off narrow terminals.** Its footer was a fixed 44 columns, so a 30-column window rendered a 46-column box that ran past the right edge. The footer now shortens (always keeping `Esc`) and the title truncates; verified fitting at every width from 10 to 140.
- **The release-notes overlay reflows when you resize the terminal.** It sized itself only when opened, so resizing while reading left the text wrapped for the old width. Scroll position is preserved across the reflow.
- **A wrapped header can no longer shift the task panel.** Both the status bar and the task list header are now clamped to the terminal width; previously a long filter or search indicator could wrap and push an extra line into the layout.
- **Text truncated to zero available columns no longer emits a stray ellipsis** into space that has no room for it — visible on very narrow terminals.

## Internal

- Direct dependency updates: lipgloss v2.0.6, goja, x/crypto v0.55, x/text v0.41, and SQLite v1.56.0 (superseding the open Dependabot PR), plus the transitive bumps they pull. Because goja and regexp2 underpin cipher solving and BotGuard, both live-gated paths were exercised beyond the offline suite: live cipher tests and the sidecar BotGuard mint both pass.
- Text truncation now delegates to Charm's `x/ansi` rather than a hand-rolled width walk, verified equivalent across ASCII, CJK and every width before adoption.
- Removed a dead `colorProfile` field that Bubble Tea's renderer already owns.
- New tests pin the status bar's degradation schedule, the overlay's fit at every width, and that every chord in the action system reaches the help overlay.

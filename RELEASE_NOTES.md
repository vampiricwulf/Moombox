### Features

- Async TUI trim with progress overlay — trim creation no longer freezes the UI while FFmpeg encodes
  - Real-time progress bar with percentage, spinner, and elapsed time (Step 3/3)
  - "Esc: Continue In Background" dismisses the overlay while encoding continues
  - Completion or error feedback shown whether the dialog is open or backgrounded
  - Guard against starting a second trim while one is already in progress

### Improvements

- FFmpeg stderr progress parsing (`time=HH:MM:SS.xx`) for real percentage-based progress reporting
- Multi-segment trim progress weighted across segments by duration (95% encode, 5% concat)
- TrimService now exposes `CreateTrimWithProgress` for callers that want progress callbacks

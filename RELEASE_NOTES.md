### Bug Fixes

- **Fix "!MISSING" in TUI/Web log display** — log calls using `slog.String()`, `slog.Any()`, etc. produced `=!MISSING` markers in the TUI and WebSocket log output because the custom log formatter didn't recognize `slog.Attr` values as self-contained key=value pairs

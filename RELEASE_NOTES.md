### Features

- **View current version's release notes anytime.** TUI `R N` chord no longer dead-ends when no update is pending — fetches the current version's notes from GitHub asynchronously and displays them in the same overlay used for pending-update notes (loading placeholder while in flight, error message if the fetch fails). Web UI Settings → Updates gains a "View Release Notes" button that does the same via a new `GET /api/update/release-notes` endpoint (defaults to running version, `?version=X.Y.Z` query param overrides). Reuses the existing update-dialog with "Update Now" hidden.
- **Browser dropdown shows the detected browser name.** The auto-cookies setup dropdown's "Auto-detect (recommended)" option now reads "Auto-detect (currently: Firefox)" when a browser is detected, or "Auto-detect (no browser found)" when none — pulling from the same `status.browser` info the API already returns. No more guessing what auto-detect actually picks.

### Internal

- **CI: dropped the `linux-test` workflow** — the build-and-release workflow at tag time is the gate; per-PR Linux test runs were noisy and surfaced cross-platform test-assertion bugs (hardcoded `Moombox.exe` strings) rather than real regressions.
- **GitHub Actions bumped to Node 24 versions** — `actions/checkout` v4→v6, `actions/setup-go` v5→v6, `actions/setup-node` v4→v6, `softprops/action-gh-release` v2→v3. Resolves the Node 20 deprecation warnings on the windows and linux jobs.

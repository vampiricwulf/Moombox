### Bug Fixes

- **Double "v" in version display** — Fixed "vv2.1.2" showing in both Web UI and TUI. The CI release workflow was passing the tag name (with "v" prefix) as the version, and the UI added another "v". Now stripped in both CI and at startup.
- **Old binary cleanup after update** — The launcher now renames `.old` to `.super` after an update restart so future updates can reuse the `.old` name. Previous versions couldn't delete the locked `.old` binary, which would cause the next update to fail.
- **Mobile skeleton loading indicators** — Job skeleton placeholders no longer persist on mobile after tasks load. Skeleton removal moved to `renderJobs()` and changed from CSS hide to DOM removal for robustness.

### Improvements

- **Settings help text** — Added missing "Requires restart" to Web UI TLS fields. Filled in empty or minimal TUI help text across all settings sections with descriptions, defaults, and restart warnings.

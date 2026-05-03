### Bug Fixes

- **TUI `R N` chord no longer reports "invalid chord" when no update is available.** The 2.6.4 fix that made R N fetch the current version's release notes was incomplete — `buildMenuItems()` (which the chord parser uses as the source of truth for valid chords) only registered R N when an update was pending, so the dispatch never reached the no-update branch. Now registered whenever either a pending-update or the `OnFetchReleaseNotes` callback can produce notes; menu label varies between "View Release Notes <new tag>" and "View Release Notes (current version)" so you can see which path will fire.

### Internal

- **CI: parallelized windows + linux builds.** Restructured the release workflow into 3 jobs: a fast `release` job (~30s, ubuntu-latest, no Go/Node compile) creates the GitHub release with the multi-platform body; `windows` and `linux` jobs then run in parallel and append their assets. Saves ~3-5 minutes wall-clock per release tag versus the previous sequential `linux needs: windows` arrangement.

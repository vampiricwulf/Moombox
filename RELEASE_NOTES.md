## Bug Fixes

- **TUI Orphaned overlay (`A O`): a failed load in one section no longer hides the other.** If the orphaned-files scan failed, the overlay showed only the error and hid any orphaned history entries that had loaded fine — and a failed history load was silently dropped. Files and history now load independently: whichever succeeds is shown, with any failure surfaced as an inline warning. Also fixed a just-deleted orphaned file briefly reappearing after you removed a history entry in the same session.

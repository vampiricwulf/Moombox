## Features

- **Members-only stream discovery.** Moombox now catches members-only YouTube live streams that the public RSS feed never lists — the reason they were previously missed even with valid member cookies. Each check cycle it scans the membership tab of every YouTube channel you have login cookies for, automatically detecting which channels you're actually a member of, with no per-channel setup. For channels set to also archive uploads & premieres, members-only VODs and premieres are captured too. Toggle it under Settings → Monitors → **Membership Stream Discovery** (on by default).
- **Automatic rollback on a failed update.** If the first boot after an update fails, Moombox now restores the previous working version automatically instead of leaving you stranded on a broken build.

## Improvements

- **Open the project's GitHub page** straight from the version indicator in the Web UI, or the `O G` chord in the TUI.
- **Task Manager now shows "Moombox Stream Archiver"** as the process name instead of a generic one.
- The launcher logs a light INFO note when it finds a stale supervisor process.

## Bug Fixes

- The release-notes viewer's **Close** button no longer skips a version.

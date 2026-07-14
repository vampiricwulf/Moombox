> Supersedes 2.7.1, which was withdrawn — see the members-only download fix under Bug Fixes.

## Features

- **Members-only stream discovery.** Moombox now catches members-only YouTube live streams that the public RSS feed never lists — the reason they were previously missed even with valid member cookies. Each check cycle it scans the membership tab of every YouTube channel you have login cookies for, automatically detecting which channels you're actually a member of, with no per-channel setup. Members items are merged with the regular RSS feed into one list ranked by recency and capped at your Max Feed Items, so a live members stream is always caught while the queue stays bounded. For channels set to also archive uploads & premieres, recent members-only VODs are captured too. Toggle it under Settings → Monitors → **Membership Stream Discovery** (on by default).
- **Automatic rollback on a failed update.** If the first boot after an update fails, Moombox now restores the previous working version automatically instead of leaving you stranded on a broken build.

## Improvements

- **Open the project's GitHub page** straight from the version indicator in the Web UI, or the `O G` chord in the TUI.
- **Task Manager now shows "Moombox Stream Archiver"** as the process name instead of a generic one.
- The launcher logs a light INFO note when it finds a stale supervisor process.

## Bug Fixes

- **Members-only discovery no longer floods downloads (fixes 2.7.1).** The initial members-only discovery release probed members-only videos *without* your login cookies, so it got no format data and misclassified every members video — even years-old VODs — as an "upcoming" stream. Because "upcoming" bypasses each channel's "archive uploads & premieres" setting, a whole channel's members-only back-catalog could be queued and downloaded at once. Members videos are now probed **with your cookies** and classified correctly (a members VOD is a VOD — skipped unless you archive uploads on that channel; a live one is caught), and RSS + members items are merged and capped at Max Feed Items so at most that many of a channel's newest items are ever processed per cycle.
- The release-notes viewer's **Close** button no longer skips a version.

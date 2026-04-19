### Improvements

- **Discord notifications use Discord timestamps** — times now render in each viewer's local timezone via `<t:UNIX:f>` format instead of plain UTC text
- **Combined "Stream Live" into "Download Starting"** — fewer redundant notifications; live stream description now says "Now live — beginning download" for both YouTube and Twitch
- **Suppressed spurious "Schedule Changed" on stream go-live** — YouTube updates the scheduled time to actual start when going live, which no longer triggers a false reschedule notification
- **Enriched multi-segment "Download Finished"** — now includes Resolution, Total Time, and Chat Messages (parity with single-file version)
- **Human-readable format labels** — YouTube "Download Starting" shows quality labels (e.g. "1080p60") instead of raw itag numbers
- **"Scheduled For" field on YouTube downloads** — "Download Starting" now shows the scheduled start time as a Discord timestamp
- **Twitch "Download Starting" now includes Category**
- **Platform prefixes on all notifications** — YouTube notifications now prefixed with "YouTube" (matching existing "Twitch" prefix convention)
- **"Download Cancelled" uses correct color** — now uses dedicated Orange (`TypeCancelled`) instead of Yellow (`TypeWarning`)
- **Populated missing fields** — "Download Cancelled" (Channel, Video ID, Thumbnail), "Trim Failed" (Channel, Video ID, Error, Thumbnail), "Trim Deleted" (Thumbnail)
- **Removed orphaned "live" event** from notification event filter lists in TUI and Web UI settings

## Features

- **Clear orphaned history entries.** The Web UI's **Files** tab is now **Orphaned** and, alongside orphaned files, lists processing-history entries that no longer have a job — most often a job you deleted. While such an entry lingers, the monitor treats that video as already-processed and never re-discovers it, so a wanted stream can stay invisible even after you re-enable its channel's uploads. Remove them from the Orphaned tab, or from the TUI's `A O` overlay, to let the monitor pick the video up again.

## Improvements

- **Clearer skip logging.** When the monitor skips an ended stream or non-stream upload, the log now says *why* — "VODs not archived for this channel" versus "already processed (in history)" — instead of one ambiguous message that made the two very different causes look the same.

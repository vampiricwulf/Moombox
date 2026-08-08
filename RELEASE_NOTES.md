## Features

- **Truncated recordings are now visible and recoverable.** A post-live download that finishes while segments it knows exist are still missing no longer reports a clean 100% success. The job is flagged (new schema v17 column `incomplete_tail`), keeps its staging directory and resume sidecar instead of having them cleaned up, and shows an "Incomplete tail" badge in both the web dashboard and the TUI. Resume — the web button, the batch bar, or the TUI's `A R` chord — appends just the missing tail once YouTube finishes processing, and the flag clears itself on a clean re-run. The progress bar and tooltip report the real segment position rather than a full bar.
- **Long post-live downloads survive URL expiry.** Googlevideo URLs live about six hours; a large backfill can outlast one. The post-live path now re-extracts fresh URLs and continues from the last written segment, up to four attempts (roughly a day of wall clock), instead of stopping at the first expiry. If the refresh would switch to a different video format mid-file, it declines and reports the recording as incomplete rather than appending mixed codecs.
- **Marathon streams fail truthfully instead of mysteriously.** YouTube keeps roughly 120 hours of a live stream's segments; a broadcast older than that has its earliest segments evicted, and archiving from the start previously died within seconds behind a generic "file too small" error. Moombox now bisects for the true retention boundary, inspects a segment there, and reports exactly how much of the broadcast is unrecoverable.

## Improvements

- **Manifest-free live downloading is now the primary path.** Following yt-dlp, live and post-live YouTube streams are fetched from their adaptive format URLs with `&sq=N` segment addressing rather than through a DASH manifest, which upstream now treats as unsupported for live. The live head sequence is harvested from the `X-Head-Seqnum` header on ordinary segment responses, so a healthy stream needs no separate probe requests, and the quality monitor selects in memory instead of re-fetching a manifest every 30 seconds. The manifest path remains as a fallback.
- **Innertube client versions refreshed** to yt-dlp's July 2026 set (TVHTML5, WEB, WEB_CREATOR, WEB_REMIX, iOS, Android), along with the desktop Chrome User-Agent window.
- **BotGuard sidecar updated to bgutils-js v4** (with jsdom 30), and dependency bumps across chi, SQLite, goldmark, and the CI actions.

## Bug Fixes

- **Silent truncation during YouTube's post-live processing window.** A transient 403 burst while YouTube was still processing a just-ended stream would finalize the recording early and report success. The downloader now defers an "ended" verdict while the head says more segments exist, retries within its timeout budget, and warns loudly if a tail genuinely can't be fetched.
- **`/retry` could destroy a flagged job's preserved recording.** The retry route restarts a job from scratch and deletes its staging directory — the opposite of what an incomplete-tail job needs. It no longer accepts flagged jobs; Resume is the correct action and is unaffected.
- **The orphan scanner could delete staging a job still needed.** Both its listing pass and its pre-delete safety check treated every finished job's staging as disposable, including directories deliberately kept for tail recovery or for parts still awaiting mux. Both now refuse them.
- **Live recordings truncated by the timeout backstop** are now flagged like post-live ones instead of finishing silently.
- **Stale downloader handles after a live quality change.** The live loop swaps downloaders on a quality refresh, but the caller kept the old ones — so end-of-job progress sync and muxing could read from superseded handles.
- Head-sequence tracking is monotonic and self-correcting, so a stale or bogus CDN reading can no longer disarm the truncation guard or force every stall into a refresh.

## Internal

- The incomplete-tail work was specified as a written plan and executed task-by-task with per-task review, a whole-branch review, and adversarial review rounds on the download state machine; the findings from each round are in the git history.
- First end-to-end coverage of the DASH download loop, driven by a fake googlevideo endpoint: clean post-live completion, transient-403 recovery, and permanent tail loss.
- New `InspectSegment` MP4 box-inventory helper and a bisecting segment-availability prober, both diagnostic-only.
- Documentation updated across `SPEC.md` and `docs/spec/` for schema v17, the refresh loop, and eviction diagnosis; the metrics appendix was re-derived from the repository and now carries its own regeneration command.

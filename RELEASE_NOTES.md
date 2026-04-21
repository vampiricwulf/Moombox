### Bug Fixes

- **Fix monitors not re-detecting restarted streams** — When a YouTube stream stalled, finished muxing, and then went live again on the same URL, both the Feed and DECAPI monitors would skip it due to the permanent history table blocklist and last-video comparison. Monitors now only gate on active jobs, allowing re-detection of streams that restart on the same URL.

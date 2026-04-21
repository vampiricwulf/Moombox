### Improvements

- **Quiet re-probe logs** — When monitors re-check previously processed videos (to catch stream restarts), log messages are now demoted to DEBUG instead of spamming INFO every cycle. Live detections still log at INFO. Re-probes of VODs/non-streams on `include_non_live_content` channels now short-circuit instead of repeatedly firing "Video found" events.

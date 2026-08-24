### Features

- **Twitch fMP4/CMAF support** — Twitch is migrating live delivery from MPEG-TS to fMP4; recordings of fMP4 streams previously failed at mux with unusable output. The HLS engine now writes the `#EXT-X-MAP` init segment at the head of every part file, tolerates Twitch's token-rotated init URLs (content-hash identity), and part-splits cleanly on a genuine mid-broadcast init change or an fMP4→TS reversion. TS streams are unaffected.
- **Broadcast interruption resume** — a new `downloader.interruption_timeout` setting (default 2h, 0 disables): a live download cut off mid-broadcast (stream crash, network death) can stall-and-resume instead of finalizing immediately, with interruption classification, chat-open resume signaling, and Tier-2 staging preservation.
- **Lossless part merging** — contiguous same-format parts are merged at finalize via lossless concat (stream-params probe gated), collapsing multi-part recordings back into fewer files, with per-part chat and segment rows collapsed to match.
- **Auto-resume on live re-detection** — a preserved Finished job whose broadcast turns out to still be live is automatically resumed by the monitor.
- **Docker support** — containerized deployment (`Dockerfile` + `ghcr.io` publish workflow) with a LAN-accessible seeded configuration by default.

### Improvements

- Quality-split notifications now also fire when a Twitch transcode restart changes quality across an init-segment part split.
- Staging expiry and offline stall-clock pause rulings applied (staged data of finalized jobs expires; the interruption stall clock pauses while offline).

### Bug Fixes

- Twitch/YouTube cookie auth recovery now fires on startup-dead auth (not only witnessed transitions) and recovers per-platform instead of service-wide.
- Stuck-segment recovery on fMP4 streams records the gap exactly once instead of split-cycling junk parts; legacy staged data from pre-fMP4 versions splits safely instead of mixing containers.
- Interruption-resume audit wave: chat-merge run-abort, `interruption_timeout=0` latch-without-stall, per-episode interruption clock, auth-walled probe mis-arm guard, finalize-scoped resume waits, part-merge cleanup safety, and duplicate resume-notification suppression.
- `ReplaceJobSegments` binds the job ID parameter correctly during part-row collapse.
- Cookie-less (anonymous) hardening: the visitor-data cache now refills from the public extraction path after 403 recovery (previously one 403 left an anonymous process visitor-less for its lifetime); a bot-checked status probe ("Sign in to confirm you're not a bot") can no longer finalize a still-live recording early; EU consent-wall redirects on watch-page fetches are detected and reported instead of silently degrading extraction; age-restricted errors now say that cookies are the fix.
- Twitch subscriber-only content (usher `unauthorized_entitlements` / `vod_manifest_restricted`) is classified with an actionable "log into an account that has access" message and routed to the COOKIES? status instead of a generic error with a raw JSON body.

### Internal

- Hermetic integration tests for chat `LiveContinuationOpen` signal wiring and ordering; expanded fMP4 engine test coverage (init ordering, rotation, reversion, ad-break inits, update-path resume pins).

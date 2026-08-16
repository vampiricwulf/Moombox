## Features

- **Configurable segment concurrency.** New `downloader.segment_workers` (default 12, minimum 1) sets how many segments one download fetches in parallel — in `config.toml`, the web settings, and the TUI, applied without restart. There is deliberately no upper limit; values above 16 log a warning, because unusually aggressive request rates carry bot-detection risk. Memory stays bounded regardless of the setting: catch-up holds at most 256 MB of out-of-order segments per stream and pauses fetching rather than ballooning.
- **Live downloads recover from credential expiry in place.** A 403 burst on segments that demonstrably exist (below the stream's advertised head) now triggers an in-process refresh — fresh player response, fresh stream URL, re-minted PO token — and the failing segments retry with the new credentials, replacing the manual cancel-and-resume workaround. Refreshes are cooldown-gated so a burst costs one round trip, and each segment's retry window is sized to outlive the cooldown so it actually sees the fresh credentials.
- **PO-token minting is fully instrumented, and the premiere-killing client mismatch is fixed.** Every GVS mint now logs its provenance — binding, challenge source, and job ID — so a 403 can be traced to its credential source, and the client-ranking fix (below) stops premiere and live URLs from being sourced from a client whose PO token doesn't match, the diagnosed cause of premiere archives producing empty streams. The full yt-dlp attestation stack (watch-page challenge extraction, challenge-sourced minters, yt-dlp's content-binding rule) ships in this build but stays dormant: the proven visitorData binding remains active until each new variable is validated against a live capture in isolation.

## Improvements

- **Catch-up is substantially faster.** The parallel catch-up loop is now a rolling window: workers claim the next sequence continuously and segments flush to disk in order as they arrive, removing the per-batch stall on the slowest segment and the per-batch head probe. Separately, the YouTube client ranking now matches yt-dlp exactly (TV first, ANDROID_VR last) — mismatched client/token pairs were the root cause of the 403 storms that made mid-stream joins crawl.
- **The speed readout measures the network, not the disk.** Speed derives from bytes arriving off the wire — including catch-up segments still waiting to be written in order — averaged over a 5-second sliding window, instead of an instantaneous flush-rate sample that sawtoothed between zero and spikes.
- **A busy download no longer reads as frozen.** "Waiting..." lines are suppressed while segment data is arriving, so a wide catch-up between ordered flushes shows its counter and live speed instead of a blank-speed wait message.
- **The progress line orders video before audio** (`(V: x A: y)`), matching the details panes' Segments row so the two never read as different counts.
- **Post-failure catch-up throttling was retuned.** The window floor is one full worker wave and regrows every second — the previous tune (copied from moonarchive's heartbeat-clocked constants) collapsed catch-up to 1–3 segments during exactly the recovery episodes it exists for.

## Bug Fixes

- **Security: the BotGuard interpreter gate is hardened.** The sidecar executes the interpreter script it fetches, so the gate now requires an exact allowlist of eight Google-owned hosts, a static `.js` path with no query, fragment, or redirects, and a canonicalized challenge object. Adversarial review defeated two earlier versions of this gate (a registrable-domain suffix bypass and a JSONP reflection returning attacker text with HTTP 200) before this one held. Updating is recommended.
- **Cancelling a job during catch-up could hang its download goroutine** when a worker sat blocked at the reorder buffer's memory ceiling; the cancel path now resolves the blocked claim and the pool unwinds.
- **A credential refresh can no longer corrupt whole-file VOD downloads** — refreshed segmented URLs are refused for downloads addressed by byte range rather than sequence number.
- **Genuinely evicted segments (410) no longer burn credential refreshes.** Only 403s — the stale-credential signature — trigger the refresh path; marathon-stream eviction bursts keep their retry budget for segments that can still be saved.
- **Failure damping no longer punishes recovery.** The 403 retry window (five attempts, doubling backoff) outlives the refresh cooldown, so a segment that starts failing right after a refresh still gets one instead of being declared permanently gone.

## Internal

- All three work streams — attestation PO tokens, 403 recovery, catch-up throughput — were executed from written plans with per-task and whole-branch reviews; the interpreter gate additionally went through adversarial security rounds whose findings are in the git history.
- New end-to-end engine coverage against a fake googlevideo endpoint: credential-expiry recovery, catch-up write ordering under the byte ceiling, deadlock-freedom on permanent gaps and cancel-at-ceiling, and a rolling-window throughput regression harness.
- Ported yt-dlp's `js_to_json` (arbitrary-precision integers, documented regex divergences) for challenge extraction.
- Engine downloaders log with job ID and stream (video/audio) on every line, so 403 investigations no longer guess at which job was failing.

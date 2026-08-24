### Features

- **Session-coherent PO token minting** — YouTube now binds its attestation challenge to the page session (`yt.config_.EVENT_ID`) and rejects WebPO tokens minted from the old `/att/get` flow when an account is enrolled in that experiment. The symptom is distinctive: player requests succeed while every segment fetch from googlevideo returns 403, which is exactly how the 2026-08-14 premiere capture failed. The BotGuard sidecar now mints from a self-consistent `(ytcfg, window.ytAtN)` pair taken from a single youtube.com fetch, so the token matches the session it was issued for. `/att/get` remains as a fallback, and every degradation step is logged with a distinct reason.
- **YouTube client roster refresh (yt-dlp 2026.08.19 parity)** — adds the `VISIONOS` client, now upstream's lead default, and puts `web_embedded` at the head of the authenticated cascade. YouTube began 403'ing `android_vr` format URLs on 2026-08-17; that enforcement is selective rather than universal, so `android_vr` is retained behind `VISIONOS` as a last-resort tier and as the only cookieless source of a live DASH manifest.

### Improvements

- Anonymous live extractions make one fewer request: a live response that already ships split video+audio adaptive formats is segment-addressable through the manifest-free path and no longer triggers a second client call chasing a DASH manifest it would never read.
- Age-restricted content no longer fetches the embedded player twice (plus an embed page) on every quality-monitor poll — the cascade result is reused when it is already usable. That path polls every 30 seconds, so it was two redundant requests per poll.
- The sidecar now enforces a whole-generation time budget rather than three independent per-fetch timeouts, which together could exceed the parent's RPC deadline. The homepage fetch carries its own tight cap so it can never consume a mid-download credential-refresh budget.
- Format deduplication ranks `VISIONOS` above `android_vr`, so a 403-dead URL cannot displace a working one when both clients return the same itag.

### Bug Fixes

- **Non-English locales misclassified every probed stream.** Stream classification matches English text in YouTube's playability reasons ("members", "age", "private"), but the two cookieless clients — including the one serving the status probe — never requested English. Both now send `hl=en`, as every other client already did.
- A crafted video title containing the literal text `ytcfg.set({` could silently disable session-coherent minting for an entire install, quietly reverting it to the very `/att/get` path that produces the 403s above. Both page extractors now evaluate every candidate instead of stopping at the first match.
- A failed homepage fetch could leave an hours-stale `EVENT_ID` paired with an unrelated challenge — the exact incoherence the feature exists to prevent. Pairing is now all-or-nothing, cleared before each attempt, and reasserted immediately before the BotGuard snapshot.
- Failed token-minter generation no longer emits a spurious warning-level unhandled-rejection line alongside the real error.

### Internal

- New live-extraction test (`MOOMBOX_LIVE_YT_TEST=1`) exercising the real anonymous cascade against YouTube for both a VOD and a live stream. It asserts capabilities — playable video plus audio, and segment addressability from either source — rather than which client or mechanism supplied them, so a healthy client reordering cannot fail it.
- New sidecar extraction test suite covering the homepage parser against hostile and live-shaped inputs, including two shapes only the real page exhibits (`\xNN`-escaped payloads and a trailing comma).
- `HasSplitAdaptiveFormats` consolidated into a single implementation shared by the extraction cascade and the download strategy switch.
- Deep-dive documentation updated for the client roster, the cookieless fallback chain, and the homepage pair's trust boundary.

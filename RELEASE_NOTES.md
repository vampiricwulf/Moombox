## Features

- **Configurable soft memory limits** for both the Go process and the BotGuard sidecar via a new `[memory]` config section. Defaults: `go_soft_limit_mb = 100`, `sidecar_soft_limit_mb = 100`, `sidecar_hard_limit_mb = 256`.
  - **Go side**: `debug.SetMemoryLimit(N)` applied at startup. Soft cap — Go GC ramps up as memory approaches the limit, but real allocations still succeed (no OOM panic).
  - **Sidecar side**: V8 has no soft-limit primitive, so the sidecar gets a hard ceiling (`--max-old-space-size`) plus proactive GC triggers. New `triggerGC` JSON-RPC method runs `globalThis.gc()`; the existing 2-minute memory-log loop fires it when sidecar RSS crosses the soft threshold. Hard ≫ soft default (256 / 100 MB) leaves enough headroom that a transient allocation won't OOM-abort the sidecar between checks.
  - Setting any knob to 0 disables that specific cap (Go uses unbounded behaviour; sidecar uses V8's default).

## Bug Fixes

- **Resume URL check no longer fires "URL mismatch, starting fresh" on every restart during an active live download.** The previous full-URL equality compare always failed because YouTube rotates session params (`expire`, `ei`, `ip`, `ns`, `n`, `sig`, `pot`, `mt`, `mh`, `lsig`, …) on every fresh manifest fetch. The check now compares on stream identity (`videoID/itag` extracted from the URL path); rotated session params no longer trigger a fresh start, preserving the precise `BytesWritten` counter and the truncate-on-resume safety net.

## Internal — review-driven cleanups (review C1–C3, S2–S4, N1–N3, N5)

- **HLS POT-recovery wiring** (review C2). HLS `OnCipherFailure` now fires on first 403 and invalidates POT / visitor-data / cipher caches. The variant URL has POT in path so we can't hot-swap mid-loop; the next orchestrator-driven refresh rebuilds the strategy with fresh values. New `invalidate403Caches` shared helper in `worker/strategies.go` consolidates DASH video / DASH audio / HLS callback bodies and enforces correct invalidation order (POT caches → visitor data) to avoid a known race window.
- **Quality probe resilience** (review C1). When the public-stream `ProbeVideoStatus` returns OK without `DashManifestURL`, the probe falls back once to authenticated `GetVideoInfo` so quality changes can't go silently undetected.
- **Visitor-data TTL** (review C3 + N5). `Service.SetVisitorData` is sticky-with-TTL: writes accept a new value only when no value is cached or the cached value is older than `visitorDataTTL` (6 h, matches the integrity-token rotation window). Long-running 24/7 sessions can rotate eventually without an explicit invalidation. New service test file covers sticky semantics, TTL refresh, and `InvalidateVisitorData`.
- **`GetVideoInfoPublic` ANDROID_VR DASH-only fallback** (review S2). Public live streams hit by the same YouTube account experiment that strips `dashManifestUrl` from cookied clients (yt-dlp issue #15274) now also benefit from the ANDROID_VR enrichment, mirroring the authenticated path.
- **Memory log: distinguish sidecar-down from sidecar-disabled** (review S4). Append `Sidecar: stats unavailable` when the sidecar is configured but its `MemoryStats` call errors out, instead of silently dropping the suffix.
- **Doc drift** (review N1–N3). `docs/spec/platform-services.md` and `architecture.md` updated to reflect sticky-with-TTL visitor data semantics, the ANDROID_VR-routed quality probe, the public-path DASH fallback, and the new `[memory]` section.

## Bug Fixes

- Visitor data is now sticky in `youtube.Service` — `SetVisitorData` only writes when no value is cached, so watch-page fetches on the hot path no longer overwrite it with each YouTube-rotated value. Previously, every quality probe / periodic full fetch produced a new POT content binding and missed the session cache, triggering a sidecar mint per probe. YouTube rotates `VisitorData` per response but the previously-issued value remains valid; pinning one per session matches yt-dlp's behaviour and lets the POT cache do its job.
- DASH 403 bursts now also invalidate POT caches and visitor data, not just the cipher solver. The `OnCipherFailure` callback can't distinguish cipher rotation from POT expiry, so it now handles both. Without this, sticky visitor data could keep a stale POT alive indefinitely.

## Improvements

- **Quality probe via ANDROID_VR for public streams.** The 30-second quality monitor was calling `GetVideoInfo` (full authenticated player API: cookies + POT) just to read `DashManifestURL` and discard everything else. `buildYouTubeProbeFn` now takes `requiresAuth`, derived from `videoInfo.PlayabilityError` at construction time:
  - members-only / age-restricted / login-required → `GetVideoInfo` (authenticated)
  - everything else → `ProbeVideoStatus` via ANDROID_VR (cookieless, no POT, no watch-page fetch)
  - Public streams (the vast majority) now run the 30-second probe at effectively zero sidecar cost. Auth-required streams still pay the full cost, but POT cache hits make repeat calls cheap.

## Internal

- New `youtube.Service.InvalidateVisitorData()` method.

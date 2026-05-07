## Bug Fixes

- HLS strategy now decrypts the throttling `n` parameter on the master URL via the routed cipher solver before fetching, matching what yt-dlp does in `extractor/youtube/_video.py:3684–3690`. On `cb017549`-family streams whose master URL ships with `/n/<encrypted>/` in its path, master/variant playlist fetches succeed (YouTube only enforces `n` on segment requests), but every segment 403'd. The bug had been latent since HLS-only streams were rare in practice — DASH was the usual path.

## Improvements

- **ANDROID_VR DASH fallback** for the YouTube account experiment that strips `dashManifestUrl` from authenticated clients (yt-dlp issue #15274). When `TV_DOWNGRADED` and `WEB_SAFARI`/`WEB` both return no DASH manifest on a live or upcoming stream, Moombox now falls back to the cookieless `ANDROID_VR` client to source DASH. Skipped for members-only / age-restricted / login-required streams (which would 401 on the anonymous client). Live-from-start segment addressability — which HLS in YouTube live cannot do — is preserved on affected accounts.
- ANDROID_VR formats from the fallback are merged into the format pool with auth-level dedup; cookied formats win same-itag ties so any Premium-tier formats from the authenticated path are preserved.

## Internal

- `DownloadHls` signature now takes the routed `cipher.Solver` and `*cipher.GojaResolver` alongside `PotProvider` and `IsOnline`. Wired through `StrategyDeps`, `hlsStrategyT.Download`, and the three quality-recovery / quality-split call sites in `orchestrator_youtube.go`.

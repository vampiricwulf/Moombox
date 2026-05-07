## Features

- **Manifest-free DASH strategy** — covers the case the v2.6.18 ANDROID_VR fallback couldn't reach. YouTube has been running an account-based experiment that strips `dashManifestUrl` from cookied client responses (yt-dlp issue #15274). For *public* streams under the experiment, we already fall back to ANDROID_VR (cookieless, unaffected by the experiment). For *members-only / age-restricted / login-required* streams, ANDROID_VR returns "Private video" — those genuinely need cookied auth, which is exactly the path the experiment is pruning.
  - The watch-page player response still ships `streamingData.adaptiveFormats[]` with full per-itag URLs even when the top-level `dashManifestUrl` is missing. The new strategy fetches each itag's URL with `&sq=N` from broadcast start, same shape moonarchive uses.
  - n-param decryption routes through `cipher.RoutedDecryptNInURL` — EJS sidecar primary, goja fallback (same composite policy as everything else).
  - POT is bound to videoID rather than visitor data — the experiment switches GVS POT enforcement to video-id-binding for cookied clients (yt-dlp logs "Detected experiment to bind GVS PO Token to video id").
  - `OnCipherFailure` invalidation chain wired identically to the manifest-driven DASH path; shared `invalidate403Caches` helper wipes cipher solver, POT cache, and visitor data on a 403 burst.
  - Verified end-to-end against a real members-only stream: sq=0 returns HTTP 200 with valid DASH MP4 (ftyp dash iso6 avc1 mp41 box header).

## Internal

- `engine.SegmentDownloader.buildSegmentURL` auto-detects URL shape: query-style URLs (the manifest-free adaptive case) get `&sq=N`; path-style URLs (manifest-driven) keep `/sq/N`. One unit test covers the new branch.
- New `HasManifestlessDashFormats(formats []youtube.Format) bool` exported from `internal/worker` — orchestrator strategy switch uses it to pick the new strategy when `!isVod && DashManifestURL == ""` and the format pool contains both video and audio adaptive entries.
- Strategy hierarchy is now:
  ```
  useDirectVod && len(Formats) > 0       → VOD (direct VOD download)
  DashManifestURL != ""                  → DASH (manifest-driven)
  !isVod && HasManifestlessDashFormats() → ManifestlessDASH (new)
  HlsManifestURL != ""                   → HLS (consolation)
  len(Formats) > 0                       → VOD-fallback
  ```
  Manifest-free DASH preempts HLS because DASH gives us per-itag selection, separate audio (cleaner mux), and live-from-start segment addressability that YouTube live HLS cannot do. Auth-level dedup of the format pool happens during PlayerAPI parsing as before — the new strategy consumes the already-finalized pool, no change to "lowest auth wins per itag" merge semantics.
- New `tools/manifestless-dash-probe` diagnostic — kept in tree for future regression testing of the recipe.

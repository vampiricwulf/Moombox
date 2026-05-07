## Bug Fixes

- **Resume identity check now handles manifestless DASH URLs.** The check shipped in 2.6.20 used a single path-style regex (`/id/<videoID>.<n>/itag/<itag>/`) that matched manifest-driven DASH and HLS variant URLs but missed manifestless DASH adaptiveFormat URLs (which are query-style: `videoplayback?id=<videoID>.<n>&itag=<itag>&...`). For manifestless DASH downloads on every restart, both `savedID` and `currentID` extracted as `""` and the fallback compared full URLs — which always differ due to rotated session params (`expire`, `ei`, `ip`, `n`, `sig`, `pot`, `mt`, `mh`, …). Field log:
  ```
  WARN [Downloader] Resume state stream identity mismatch, starting fresh savedID="" currentID=""
  ```
  Add a query-style fallback (`[?&]id=...` + `[?&]itag=...`) that runs when path-style misses. Three new tests cover the query-style URL, identity stability across rotated session params, and different-itag-on-same-stream still differing.

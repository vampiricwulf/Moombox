### Bug Fixes

- Fix spurious quality splits when transient DASH errors (403/410) trigger `ErrQualityLost` but the stream quality hasn't actually changed — the orchestrator now re-fetches the manifest and compares quality before splitting, continuing in the same staging directory if quality is unchanged
- Fix new downloaders after a quality split starting from sequence 0 (re-downloading the entire stream) — the old downloader's sequence position is now passed to the new one via `ForceStartSeq`
- Apply same-quality skip to both YouTube and Twitch orchestrators, covering both reactive (`ErrQualityLost`) and proactive (quality monitor) trigger paths

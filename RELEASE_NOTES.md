### Features

- **Quality preference for YouTube and Twitch** — per-channel quality targeting (e.g. "1080p60", "720p", "audio_only") now works for both platforms, with automatic descent to next-lower resolution when the preferred quality isn't available
- **Quality monitoring and splitting** — live streams are monitored every 30 seconds for resolution changes; when detected, the current segment is cleanly muxed and recording continues at the new quality as separate segment files
- **Audio-only mode for YouTube** — `audio_only` quality preference now works across all three YouTube download strategies (DASH, HLS, VOD)
- **Multi-segment playback** — web player seamlessly plays across quality-split segments with global time tracking, segment indicator bar, and cross-segment seeking
- **Cross-segment trimming** — trim dialog supports spans across segment boundaries with automatic resolution normalization and FFmpeg concatenation

### Improvements

- Quality preference UI expanded to 15 options and shown for both YouTube and Twitch channels
- Quality splits only trigger on resolution changes (not FPS-only changes) to avoid unnecessary file fragmentation
- Ignored quality changes during minimum segment duration (10s) reset the monitor baseline so improvements are re-detected once the threshold passes
- Job details panel shows quality segments with resolution, duration, and file size

### Bug Fixes

- Fixed nil pointer dereference in `IsProgressiveFormat` when video format is nil (audio-only and manual itag=-1 paths)
- Fixed stale segment indicator remaining in player DOM when switching from multi-segment to single-file job
- Fixed reactive quality loss within minimum segment duration silently stopping Twitch recordings while stream was still live

### Internal

- New database schema v5: `quality_preference` column on jobs, `segments` table with 13 fields
- New `QualityMonitor` with mutex-protected baseline and non-blocking channel sends
- New `ErrQualityLost` sentinel error in segment downloader with 5 return sites (3 DASH, 2 HLS)
- FPS parsing added to DASH (`frameRate` attribute) and HLS (`FRAME-RATE` attribute) manifests
- `TrimAndConcat` method on muxer for multi-segment trim with intermediate re-encoding
- Segment video serving route with path traversal guard

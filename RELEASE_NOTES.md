### Bug Fixes

- **Fixed chat replay 2-minute desync** — when early chat starts during the upcoming phase, offsets are calculated against YouTube's scheduled start time. If the stream starts late, all chat offsets are inflated by the gap. The player now computes a timing correction from the first segment's capture time (multi-segment) or the job's actual start time (single-file), correcting both old and new chat files during playback

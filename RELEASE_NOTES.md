### Features

- **One-time Twitch channel monitor** — manually adding an offline Twitch channel now waits for the stream to start instead of immediately failing. The job enters Upcoming status, polls every 15–20s, and begins downloading once the channel goes live
- **Auto-parse channel URLs** — paste YouTube or Twitch URLs directly when adding monitored channels instead of manually extracting channel IDs. Supports `youtube.com/@Handle`, `/channel/UCxxx`, `/c/Name`, `/user/Name`, and `twitch.tv/streamer` — auto-detects platform and resolves to channel ID in both web UI and TUI

### Bug Fixes

- Fixed `extractTwitchLoginFromJob` not handling `tw_manual_` video ID prefix, which could fail to extract the login for manually-added offline Twitch channels with underscores in their name

### Bug Fixes

- **Active Platforms** no longer defaults YouTube to enabled when no cookies are set up. Platforms now remember which were configured via auto-cookie setup, and cookie file validation auto-detects platforms on first load.
- **Feed monitor** no longer polls when no YouTube channels are configured, matching the existing behavior of the DECAPI and Twitch monitors. Monitors wake up immediately when channels are added.

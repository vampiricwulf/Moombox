### Features

- **Cookie system revamp**: Active platform detection infers YouTube/Twitch from monitored channels, with manual override toggles in settings. Unused platform indicators are hidden from status bar. Per-platform "Setup YouTube" / "Setup Twitch" buttons replace the combined 2-step auto-cookie flow.

### Bug Fixes

- **Chat count display**: Chat message count now updates independently in the progress tracker, instead of only updating when video/audio segments arrive.
- **yt-dlp plugin HTTPS**: Generated yt-dlp PO token plugin now respects the HTTPS setting, including SSL context bypass for self-signed localhost certs.
- **yt-dlp plugin mismatch detection**: Plugin status checks now detect both port and scheme (HTTP/HTTPS) mismatches, not just port. Web UI extractor command also uses the correct scheme.

### Internal

- Simplified release workflow with pre-generated release notes.

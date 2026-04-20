### Features

- **Internet connectivity awareness** — Moombox now detects when your device loses internet and acts appropriately: YouTube downloads wait and resume cleanly instead of falsely concluding streams ended; Twitch downloads immediately save what was captured (since segments are ephemeral) and auto-start a new job when connectivity returns; monitors skip polls while offline and immediately re-check on recovery
- **Offline indicators** — Web UI shows a banner ("Internet connection lost") and TUI shows an OFFLINE indicator in the status bar when connectivity is lost; both auto-clear on recovery
- **Passive connectivity backup** — In addition to the Windows system API check, cross-subsystem HTTP failure correlation detects "connected to WiFi but no internet" edge cases

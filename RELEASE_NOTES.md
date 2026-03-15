### Improvements
- Setup wizard: "Use Defaults" skip option to save default config and start immediately
- Setup wizard: interactive browser cookie login in TUI advanced mode (replaces toggle)
- Setup wizard: FFmpeg status shown on mode selection screen (TUI and Web UI)
- Setup wizard: Web UI channels can now be edited (pencil icon)
- Setup wizard: Web UI channel dialog now includes quality preference and enabled fields
- Setup wizard: TUI advanced setup now includes segment retry and live check retry fields
- Setup wizard: "Saving configuration..." overlay shown during config save (TUI)
- Setup wizard: password field uses masked input in TUI

### Bug Fixes
- Setup wizard: Ctrl+C now quits the app during setup (was silently swallowed)
- Setup wizard: cookie extraction failure now shows error feedback instead of silent reset
- Setup wizard: "External" network access option and password field added to TUI advanced setup
- Setup wizard: cookie extraction uses app context with 60s timeout (was uncancellable)
- Setup wizard: Web API setup works on config copy, prevents partial config corruption on save failure
- Setup wizard: TUI config save now holds cfgMu lock
- Setup wizard: double-Esc required to abandon TUI advanced setup (prevents accidental data loss)
- Setup wizard: duplicate channel ID validation in both TUI and Web UI
- Setup wizard: Web UI hides include_non_live checkbox for Twitch channels
- Setup wizard: Web UI cookie extraction has 60s timeout
- Setup wizard: channels preserved when navigating back from channels to cookies in advanced mode
- Setup wizard: password hashed before saving to disk (was plaintext until restart)
- Setup wizard: key input blocked during async save (prevents duplicate saves)
- Setup wizard: nil guard on auto-cookie service start callback
- Setup wizard: TUI save path creates output/staging directories (matches Web API)
- Setup wizard: quality selector shows [best] highlighted for default value

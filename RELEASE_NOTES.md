### Improvements

- Refactored browser cookie refresh to iterate over active platforms instead of hardcoding YouTube + Twitch visits — only visits platforms that have cookies in the jar, skips entirely when none are present
- Renamed `HasAuthCookies` to `HasYouTubeAuthCookies` on CookieJar for symmetry with existing `HasTwitchAuthCookies`, replaced all raw `GetCookie("auth-token")` checks with the proper method

### Bug Fixes

- Fixed Firefox "already running" error during auto-cookie refresh when both YouTube and Twitch cookies are configured — lock files are now cleaned between sequential Firefox launches

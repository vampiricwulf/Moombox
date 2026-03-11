### Bug Fixes

- **Downgraded android_vr client to v1.65.10** — newer versions may return SABR-only streams that Moombox can't handle
- **Fixed age-restricted detection** — `LOGIN_REQUIRED` with age-related reasons now correctly maps to age-restricted instead of generic login-required

### Features

- **Added web_safari client** — now the primary web client (more reliable for unauthenticated use), with standard web as fallback
- **Added web_embedded client for age-restricted content** — fetches embed page for `encryptedHostFlags`, uses non-YouTube `embedUrl` and `thirdParty` context to bypass age gates

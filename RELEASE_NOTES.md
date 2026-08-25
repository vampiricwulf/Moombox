## Features

- **Browser-free cookie import from a mounted Firefox profile.** Point `browser_profile_dir` at a profile directory (it already defaults to `/data/browser-profile` in Docker) and Moombox reads `cookies.sqlite` directly — no browser process, so it works in a container. The profile is copied with its `-wal` sidecar before reading; copying `cookies.sqlite` alone opens fine and silently returns zero cookies, which is the trap behind most "my profile has the cookies but nothing works" reports.
- **`network.trusted_proxies`** — a list of reverse-proxy IPs/CIDRs whose `X-Forwarded-For` is honored when resolving the client IP. Empty by default, hot-reloadable, exposed in config, API, Web UI and TUI.
- Members-only failures now name the actual problem, and both UIs distinguish "not a member of this channel" from "cookies are dead".

## Bug Fixes

- **Reverse proxies bypassed the IP gate and authentication entirely.** Any proxy in front of Moombox made every forwarded request — including internet traffic — appear to come from the proxy's private address, which passed the `lan` filter and skipped auth. Trust decisions now resolve through `EffectiveClientIP`, which honors `X-Forwarded-For` only from a declared trusted proxy and reads **every** header line (HAProxy appends a separate one by default, which a single-line read would have let through).
- **Members-only content was reported as a cookie failure even when the session was fine**, and pointed at a browser login that cannot run in a container. Moombox now distinguishes a signed-in-but-not-a-member account from dead cookies, names the client that produced the verdict, and stops asserting causes it cannot know.
- **Not-a-member jobs were retried forever.** The recovery sweep resumed every `COOKIES?` job on any auth transition, so a job that could never succeed re-ran on every cycle. The park reason is now persisted, and such a job resumes only when the signed-in account actually changes.
- **Firefox 142+ cookie expiry was read 1000× too large.** Firefox moved `moz_cookies.expiry` to milliseconds at schema 16; expiries are now converted, so genuinely-expired cookies stop being merged forward indefinitely.
- **Six nullable `moz_cookies` columns silently dropped rows.** A NULL in any of them failed the scan and the row vanished without a word.
- **Chrome/Edge 130+ cookie values carried 32 bytes of binary garbage** — a domain hash is now stripped when the profile's `meta.version` calls for it, and non-decryptable rows are counted and named instead of discarded.
- **Every settings save from a Docker dashboard returned 400.** Path validation rejected absolute paths while the entrypoint seeds `/data/...`, with no way to fix it from the UI. Absolute paths are accepted; `..` traversal still is not, and the rule is now shared by both UIs.
- **A cookie refresh could write an empty or partial `cookies.txt`, clear the error state and report success.** Losing one platform's credentials while another survived is now stated rather than silently swallowed.
- `network_access = "public"` was silently reset to `"localhost"` at load, and the Web UI settings panel could not save anything at all while it was set.
- Passwordless external access now warns loudly at startup and in both UIs instead of booting silently.
- `docker-compose.yml` no longer suggests bind-mounting `cookies.txt` as a single file — that breaks the atomic-rename write-back and silently disables the 30-minute YouTube session refresh.

## Improvements

- The Docker compose network is IPv6-enabled, so inbound IPv6 is handled by ip6tables rather than being re-originated from the bridge gateway's private IPv4 address and misclassified as LAN.
- Documentation: a Remote Access guide covering VPN, reverse proxy and direct exposure; Docker source-IP caveats; and corrections to several security-spec claims that did not match the code.

## Internal

- Database schema v19: `park_reason` and `park_identity` on jobs.
- Cookie-failure diagnostics throughout — decrypt failures, schema probes and dropped rows are now counted and reported rather than silently absorbed.

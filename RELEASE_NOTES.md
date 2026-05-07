## Features

- **Settings UI parity for memory + BotGuard + reverse-proxy + DPAPI**. Audit pass exposed several `MoomboxConfig` fields that were settable via `config.toml` but invisible from the TUI and Web UI:
  - **BotGuard Sidecar** — new section with an enable/disable toggle (`bgutils.use_sidecar`).
  - **Memory** — new section surfacing the `[memory]` knobs shipped in v2.6.21 (`go_soft_limit_mb`, `sidecar_soft_limit_mb`, `sidecar_hard_limit_mb`).
  - **Trust forwarded proto** — new toggle in the Network section (`network.trust_forwarded_proto`) for setting the Secure cookie flag based on `X-Forwarded-Proto` when running behind a TLS-terminating reverse proxy.
  - **DPAPI fallback (Windows)** — new toggle in the Cookies section (`cookies.dpapi_fallback`) for reading real Chromium browser cookies via Windows DPAPI when the CDP refresh path fails.
- **Auto-persist new config sections on first load**. When a user upgrades and `loadFromFile` detects an expected section missing from their existing `config.toml` (e.g., `[memory]` after upgrading from 2.6.20 → 2.6.21), Moombox now flushes the full struct — defaults included — back to disk on startup. Previously the new section stayed invisible until the user saved something through the UI; now it appears automatically.

## Internal

- New `MoomboxConfig.NeedsAutoPersist` in-memory flag (excluded from TOML/JSON marshalling) signals between `loadFromFile` and `cmd/moombox/services.go`'s init path.
- API route updates handler (`config_routes.go`) now applies `bgutils`, `network.trust_forwarded_proto`, `cookies.dpapi_fallback`, and validates `memory.*` ranges + sidecar soft-vs-hard ordering.

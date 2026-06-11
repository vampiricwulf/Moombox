# Security

## Scope

This document defines the security architecture of Moombox's HTTP server, authentication system, authorization model, and binary update verification. It covers the middleware stack, CSRF protection, session management, IP-based access control, rate limiting, Content Security Policy, TLS configuration, Ed25519 update signing, and panic recovery requirements. Every security-relevant decision and its rationale is documented here so that an AI or developer can understand and maintain the security posture without guessing.

## Rules and Constraints

These are hard rules. They are not guidelines, suggestions, or aspirations. An AI assisting with Moombox development must follow these without exception:

- **Middleware order is critical and MUST be maintained.** The middleware chain is applied in this exact order: Recovery, CORS, SecurityHeaders, CSRF, IPGate, MaxBodySize, Compression, Auth. Reordering can create security vulnerabilities (e.g., moving Auth before IPGate would break local-network trust; moving CSRF after Auth would leave authenticated routes unprotected against cross-site request forgery).
- **CSRF uses Origin/Referer validation, NOT CSRF tokens.** Moombox does not generate or validate CSRF tokens. It validates the Origin or Referer header on mutating requests (POST, PUT, DELETE) against the configured network_access level. This is sufficient because the server controls CORS preflight responses and does not grant cross-origin access to untrusted origins.
- **TUI bypasses CSRF via the X-Internal-Token header.** The TUI is a same-process client that cannot send Origin/Referer headers. It sends a 16-byte random hex token (generated at server startup) in the `X-Internal-Token` header. The comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing side-channels.
- **Loopback and private IPs skip authentication.** Requests from 127.0.0.1, ::1, and private IP ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7, link-local addresses) bypass the AuthMiddleware entirely. Authentication is only enforced for external (non-local, non-LAN) clients when a password is configured.
- **Ed25519 signature verification before binary swap.** Self-updates download a new binary and a `.sig` file. The binary is verified against the embedded Ed25519 public key before any file rename operations occur. An invalid signature aborts the update.
- **NEVER trust X-Forwarded-For.** IP extraction uses `net.SplitHostPort(r.RemoteAddr)` directly. The `ExtractIP` function explicitly ignores `X-Forwarded-For`, `X-Real-IP`, and all other proxy headers to prevent IP spoofing attacks that would bypass IP-based access control and authentication.
- **All goroutines must have panic recovery.** Every goroutine in the application — HTTP handlers, background workers, database callbacks, monitor callbacks — must include a `defer func() { if r := recover(); r != nil { ... } }()` block. A panic in one subsystem must never crash the application. This is enforced at multiple layers: RecoveryMiddleware for HTTP, `safeCallJobUpdate`/`safeCallJobsChange` for database subscribers, and inline defers for all other goroutines.

---

## Middleware Stack

The middleware chain is applied in `NewServer()` in `internal/web/server.go`. The order is load-bearing — each middleware depends on the ones before it having already executed, and moving any middleware out of position can create security gaps.

### 1. RecoveryMiddleware

**Purpose:** Catches panics in HTTP handlers and returns a structured 500 response instead of crashing the server process.

**Behavior:**
- Wraps the `http.ResponseWriter` in a `recoveryWriter` that tracks whether headers have already been sent to the client.
- If a panic occurs and headers have not been sent, it writes a `500 Internal Server Error` JSON response: `{"error":"Internal server error"}`.
- If headers have already been sent (partial response written), it cannot write a new status code — the connection is effectively broken, but the server survives.
- Logs the panic value and the request path at Error level.

**Why it is first:** If any subsequent middleware or handler panics, this middleware catches it. If it were placed later in the chain, panics in earlier middleware would crash the process.

**Source:** `RecoveryMiddleware` in `internal/web/server.go`.

### 2. CORSMiddleware

**Purpose:** Validates `Origin` headers on cross-origin requests and sets appropriate CORS response headers based on the `network_access` configuration.

**Behavior:**
- If an `Origin` header is present and the origin is allowed (determined by `isAllowedOrigin` against the `network_access` config), it sets:
  - `Access-Control-Allow-Origin` to the requesting origin (not `*`).
  - `Access-Control-Allow-Methods`: GET, POST, PUT, DELETE, OPTIONS.
  - `Access-Control-Allow-Headers`: Content-Type, Authorization, X-Requested-With.
  - `Access-Control-Allow-Credentials`: true.
  - `Access-Control-Max-Age`: 86400 (24 hours).
- OPTIONS preflight requests receive a `204 No Content` if the origin is allowed, or `403 Forbidden` if not.
- Origin validation uses `url.Parse` for proper URL parsing — no substring matching.

**Origin allowance rules by network_access level:**
- `localhost`: Only loopback IPs and `localhost`.
- `lan`: Loopback + `localhost` + private IPs.
- `external` / `public`: Any origin.
- Default (unset): Same as `localhost`.

**Source:** `CORSMiddleware` and `isAllowedOrigin` in `internal/web/middleware.go`.

### 3. SecurityHeaders

**Purpose:** Sets hardened HTTP response headers on every response to mitigate common web attacks.

**Headers set:**
- `X-Frame-Options: DENY` — Prevents the page from being embedded in iframes (clickjacking protection).
- `X-Content-Type-Options: nosniff` — Prevents MIME type sniffing.
- `Referrer-Policy: no-referrer` — Prevents the browser from sending the Referer header on navigation.
- `Permissions-Policy` — Disables sensitive browser APIs: accelerometer, camera, geolocation, gyroscope, microphone. Allows autoplay(self), clipboard-write(self), encrypted-media(self), picture-in-picture(self).
- `Content-Security-Policy` — Full CSP documented in the Content Security Policy section below.

**Source:** `SecurityHeaders` in `internal/web/middleware.go`.

### 4. CSRFMiddleware

**Purpose:** Prevents cross-site request forgery on mutating requests (POST, PUT, DELETE).

**Behavior — step by step:**
1. **Safe methods pass through.** GET, HEAD, and OPTIONS requests are never subject to CSRF validation.
2. **Loopback-only routes are exempt.** The paths `/get_pot`, `/invalidate_caches`, and `/invalidate_it` are called by external Python scripts (yt-dlp) that do not send Origin/Referer headers. These routes are already protected by `LoopbackOnly` middleware at the route level, so CSRF protection is redundant.
3. **Internal token bypass.** If the request includes an `X-Internal-Token` header whose value matches the server's startup-generated token (compared with `crypto/subtle.ConstantTimeCompare`), the request passes through. This is safe because browsers cannot set custom headers on cross-origin requests without a CORS preflight, which the server does not grant to untrusted origins.
4. **Origin/Referer required on mutating requests.** Any POST/PUT/DELETE (and other mutating method) must present either an allowed `Origin`/`Referer` header or the internal token. If neither is present, the request is rejected with `403 Forbidden: missing origin` regardless of `network_access`. Previously localhost / LAN access bypassed this check, but that allowed any local process or same-origin browser tab to call state-changing endpoints (`/api/restart`, `/api/auth/set-password`, `/api/jobs/{id}/open-folder`) without browser context. Non-browser local CLIs should set `Origin: http://localhost:<port>` or supply the internal token.
5. **Origin/Referer validation.** When a header is present, it is validated against the `network_access` config using `isAllowedOrigin`. If the origin is not allowed, the request is rejected with `403 Forbidden: invalid origin`.

**Source:** `CSRFMiddleware` in `internal/web/middleware.go`.

### 5. IPGateMiddleware

**Purpose:** Restricts HTTP access based on the `network_access` configuration level and the client's IP address.

**Behavior by network_access level:**
- `external` / `public`: All IPs allowed.
- `lan`: Only loopback (127.0.0.1, ::1) and private IPs (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7, link-local unicast). External IPs receive `403 Forbidden`.
- `localhost` (or unset default): Only loopback IPs. Everything else receives `403 Forbidden`.

**IP extraction:** Uses `ExtractIP(r)` which calls `net.SplitHostPort(r.RemoteAddr)`. Never trusts proxy headers.

**Private IP detection:** The `isPrivateIP` function checks against pre-parsed CIDR blocks (parsed once at package init to avoid per-request overhead) and also treats `IsLinkLocalUnicast()` addresses (fe80::/10 IPv6, 169.254.0.0/16 IPv4) as private, since phones on LAN often connect via IPv6 link-local.

**Additional route-level gating:** The `LoopbackOnly` middleware is applied to specific routes (`/get_pot`, `/invalidate_caches`, `/invalidate_it`) that must only be accessible from the local machine regardless of `network_access` config.

**Source:** `IPGateMiddleware`, `LoopbackOnly`, `ExtractIP`, `isPrivateIP`, `isLoopback` in `internal/web/middleware.go`.

### 6. MaxBodySize

**Purpose:** Limits the request body size on mutating requests (POST, PUT, DELETE) to prevent abuse and resource exhaustion.

**Behavior:**
- Default limit: 1 MB (1 << 20 bytes), applied to all mutating requests.
- GET, HEAD, and OPTIONS requests are not limited.
- Wraps `r.Body` with `http.MaxBytesReader`, which returns a `413 Request Entity Too Large` error when the limit is exceeded.
- Individual endpoints can override the limit by wrapping `req.Body` with their own `http.MaxBytesReader`. The import endpoint overrides to 500 MB.

**Source:** `MaxBodySize` in `internal/web/middleware.go`.

### 7. CompressionMiddleware

**Purpose:** Applies gzip compression to responses larger than 1 KB to reduce bandwidth usage.

**Behavior:**
- Only compresses if the client sends `Accept-Encoding: gzip`.
- Skips WebSocket upgrade requests (detected by `Upgrade` header).
- Skips video streaming endpoints (`/api/jobs/*/video`) to avoid buffering large media.
- Uses a buffered approach: accumulates response bytes until the 1 KB threshold is reached, then switches to gzip. Responses under 1 KB are sent uncompressed (the overhead of gzip headers would negate the savings).
- Implements `http.Flusher`, `http.Hijacker`, `http.Pusher`, and `Unwrap()` for compatibility with downstream code that expects these interfaces.

**Source:** `CompressionMiddleware` and `gzipResponseWriter` in `internal/web/server.go`.

### 8. AuthMiddleware

**Purpose:** Enforces authentication for external (non-local, non-LAN) clients when a password is configured. Applied last in the chain so that route-level middleware can execute first.

**Note on registration:** AuthMiddleware is registered separately from the other middleware — it is added via `r.Use(webServer.AuthMiddleware)` in `main.go` after the server is constructed, because it requires the AuthService to be wired up first. Despite this, it is the last middleware in the chain.

**Behavior — step by step:**
1. Extract the client IP via `ExtractIP(r)`.
2. If the IP is loopback or private: skip auth, serve the request.
3. If `IsAuthRequired` returns false (network_access is not `external`, or no password hash is configured): skip auth.
4. If the request path is a public endpoint (`/api/auth/login`, `/api/auth/status`, `/ping`, `/minter_cache`, `/favicon.svg`, `/login.html`): skip auth.
5. Check the `moombox_session` cookie. If valid (exists in the in-memory session map and not expired): serve the request.
6. Fallback: check the `moombox_client` cookie. If the `ClientTokenCheck` callback validates the persistent client token: issue a fresh session cookie and serve the request.
7. If unauthenticated:
   - API requests (`/api/*`): return `401 {"error":"Authentication required"}`.
   - Browser requests: serve `login.html` inline (preserves the URL bar instead of redirecting).

**Source:** `AuthMiddleware` in `internal/web/server.go`.

---

## CSRF Protection

Moombox uses Origin/Referer header validation rather than CSRF tokens. This decision is deliberate — CSRF tokens require server-side state management and careful integration with every form and AJAX call. Origin/Referer validation provides equivalent protection with less implementation complexity, given that:

1. The server controls CORS preflight responses and never grants `Access-Control-Allow-Origin` to untrusted origins.
2. Browsers reliably send the `Origin` header on cross-origin POST/PUT/DELETE requests.
3. The only clients that legitimately omit `Origin` are same-process clients (TUI), which authenticate via the internal token.

### Exemptions

Three categories of requests are exempt from CSRF validation:

**Safe HTTP methods:** GET, HEAD, and OPTIONS never modify state and are always exempt.

**Loopback-only routes:** `/get_pot`, `/invalidate_caches`, `/invalidate_it` are exempt because they are called by yt-dlp Python scripts that cannot send browser headers. These routes enforce `LoopbackOnly` at the route level, making CSRF protection redundant — an attacker cannot reach them from outside the local machine.

**Internal token bypass:** The TUI sends `X-Internal-Token` on every request via a custom `net/http.RoundTripper`. The CSRF middleware allows the request if the token matches (constant-time comparison). This is safe because browsers cannot set custom headers on cross-origin requests without a successful CORS preflight, and the server never grants preflight to untrusted origins.

### Missing Origin Handling

When a mutating request arrives without an Origin or Referer header and without an internal token:
- If `network_access` is `external` or `public` AND a password hash is configured: the request is rejected. This prevents blind form-submission CSRF attacks where the browser does not send an Origin header (some older browser/form combinations).
- For `localhost` or `lan` access: the request is allowed. The IP gate and auth middleware already restrict access to trusted networks.

---

## Authentication

### Password Hashing

**Algorithm:** scrypt with parameters N=16384, r=8, p=1, keyLen=64, saltLen=16. These are standard scrypt parameters that provide strong protection against brute-force attacks while remaining fast enough for interactive login (typically under 100ms on modern hardware).

**Hash format:** `scrypt:<salt_hex>:<hash_hex>` — stored in the TOML config file under `[network] password_hash`.

**Auto-hashing:** On startup, if the config contains a plaintext password (detected by checking if the value does not match the `scrypt:*:*` format via `IsScryptHash`), the server automatically hashes it with scrypt and writes the hash back to the config. This allows users to set passwords in plaintext for convenience, with automatic conversion to a secure hash.

**Verification:** Uses `crypto/subtle.ConstantTimeCompare` to compare the computed hash against the stored hash, preventing timing side-channel attacks.

**Source:** `HashPassword`, `VerifyPassword`, `IsScryptHash` in `internal/web/auth.go`.

### Session Management

**Token generation:** 32 random bytes from `crypto/rand`, hex-encoded to produce a 64-character string.

**Storage:** In-memory `map[string]sessionEntry` protected by `sync.RWMutex`. Sessions are NOT persisted to the database — they are lost on restart, requiring re-authentication. This is intentional: session persistence would add complexity without meaningful benefit, since persistent client tokens (below) handle the "remember me" use case.

**TTL:** 24 hours from creation. There is no sliding window — the session expires exactly 24 hours after it was created regardless of activity.

**Cleanup:** A background goroutine runs every hour (`sessionCleanup = 1 * time.Hour`) and evicts all sessions whose creation time is older than 24 hours.

**Cookie properties:**
- Name: `moombox_session`
- Path: `/`
- MaxAge: 86400 (24 hours)
- HttpOnly: true (inaccessible to JavaScript)
- Secure: true only if TLS is active (`r.TLS != nil`)
- SameSite: Lax

**Source:** `CreateSession`, `ValidateSession`, `SetSessionCookie`, `evictExpired` in `internal/web/auth.go`.

### Client Token Persistence

Client tokens provide a "remember this browser" mechanism for remote clients, surviving server restarts (unlike in-memory sessions).

**Token generation:** 32 random bytes, hex-encoded (64 chars) — same as session tokens.

**Token hashing:** `<salt_hex>:<hash_hex>` (note: no `scrypt:` prefix, unlike password hashes). Uses the same scrypt parameters as password hashing.

**Token prefix:** The first 8 hex characters of the raw token are stored in the database as an indexed column. This allows efficient lookup: the server finds candidate rows by prefix, then verifies the full hash. This avoids scanning all client tokens on every request.

**Rotation:** When a user re-logs in from the same browser, the old client token is revoked and a new one is issued. This limits the window of exposure if a token is compromised.

**Metadata tracking:** Each client token record stores a label (browser/OS user-agent string), the client's IP address, and a timestamp.

**Cookie properties:**
- Name: `moombox_client`
- Long-lived (persists across browser sessions)

**Auth flow integration:** When the `moombox_session` cookie is missing or invalid, the AuthMiddleware falls back to checking the `moombox_client` cookie. If the client token is valid, a fresh session is issued (new `moombox_session` cookie set on the response) and the request proceeds.

**Source:** `GenerateToken`, `TokenPrefix`, `HashToken`, `VerifyToken` in `internal/web/auth.go`. Client token database storage in `internal/database/`.

### Auth Flow Summary

For every incoming request:

1. Extract client IP from `RemoteAddr` (never from proxy headers).
2. If IP is loopback (127.0.0.1, ::1) or private (LAN ranges): **skip auth entirely**.
3. If `network_access` is not `external` or no password hash is configured: **skip auth**.
4. If the path is a public endpoint (login, status, ping, favicon): **skip auth**.
5. Check `moombox_session` cookie → validate against in-memory session map → if valid: **authenticated**.
6. Check `moombox_client` cookie → validate via `ClientTokenCheck` callback → if valid: issue fresh session, **authenticated**.
7. Otherwise: **unauthenticated**. API paths get `401 JSON`. Browser paths get `login.html` served inline.

---

## Authorization (IP-Based Access Control)

Moombox does not have role-based access control or user accounts. Authorization is binary: you either have full access or no access. The access decision is based entirely on the client's IP address and the `network_access` configuration.

### network_access Levels

| Level | Who can connect | Auth required? |
|-------|----------------|----------------|
| `localhost` (default) | Loopback only (127.0.0.1, ::1) | Never |
| `lan` | Loopback + private IPs (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7, link-local) | Never |
| `external` / `public` | Any IP | Yes, if password configured |

**Key behaviors:**
- Loopback and private IPs are always trusted regardless of the `network_access` setting. Even at `external` level, a request from 192.168.1.100 skips authentication.
- Authentication is only enforced when `network_access == "external"` AND a `password_hash` is configured. The `public` level allows any IP but does not enforce auth (it is intended for scenarios where the server is behind a reverse proxy that handles authentication).
- The IP gate (middleware layer 5) rejects connections from disallowed IP ranges before they reach the auth layer. This means that at `localhost` level, a request from an external IP never reaches the auth check — it is rejected at the network level.

### Private IP Detection

The `isPrivateIP` function uses pre-parsed CIDR blocks (allocated once at package init) to check if an IP falls within private ranges:

- `10.0.0.0/8` — Class A private
- `172.16.0.0/12` — Class B private
- `192.168.0.0/16` — Class C private
- `fc00::/7` — IPv6 unique local addresses
- `IsLinkLocalUnicast()` — fe80::/10 (IPv6) and 169.254.0.0/16 (IPv4) link-local addresses, included because mobile devices on LAN frequently connect via IPv6 link-local

Loopback detection uses Go's `net.IP.IsLoopback()`, which covers both 127.0.0.0/8 (IPv4) and ::1 (IPv6), plus the string `"localhost"` as a fallback.

---

## Rate Limiting

### Algorithm

Sliding window per-IP rate limiting, implemented entirely in-memory. Each IP address has an array of request timestamps. When a new request arrives, expired timestamps (outside the window) are filtered out. If the remaining count meets or exceeds the limit, the request is rejected.

### Memory Bounds

The rate limiter caps total per-IP entries at 10,000 (`maxRateLimiterEntries`). When this limit is exceeded, the oldest entry is evicted. This prevents an attacker from consuming unbounded memory by making requests from many distinct IPs.

A cleanup goroutine runs every 60 seconds and purges all expired entries from the map.

### Response on Rate Limit

When a request is rate-limited:
- HTTP status: `429 Too Many Requests`
- `Retry-After` header: number of seconds until the oldest request in the window expires (plus 1 second of buffer)
- JSON body: `{"error":"Too many requests, please try again later","retryAfter":N}`

### Per-Route Limits

All rate limiters use a 60-second sliding window. The limits are configured in `cmd/moombox/main.go`:

| Route | Limit | Window | Purpose |
|-------|-------|--------|---------|
| Login (`/api/auth/login`) | 5 | 60s | Brute-force protection |
| Password set/remove | 3 | 60s | Prevents rapid password changes |
| POT generation (`/get_pot`) | 10 | 60s | Limits BotGuard work (sidecar IPC + Google WAA round-trip on cache miss) |
| Import (`/api/import`) | 5 | 60s | Limits resource-intensive archive imports |
| API general | 20 | 60s | Broad rate limit on API endpoints |

**Source:** `internal/web/rate_limiter.go`, rate limiter instantiation in `cmd/moombox/main.go`.

---

## Content Security Policy

The CSP is set via the `Content-Security-Policy` header in `SecurityHeaders` middleware. It defines what resources the browser is allowed to load:

```
default-src 'self'
script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net
style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net
font-src 'self' https://cdn.jsdelivr.net
img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.jsdelivr.net https://fonts.gstatic.com
connect-src 'self' ws: wss: https://cdn.jsdelivr.net data:
frame-src https://www.youtube-nocookie.com https://player.twitch.tv
object-src 'none'
base-uri 'self'
form-action 'self'
```

### Directive Rationale

- **`script-src 'unsafe-inline'`** — Required because Shoelace components and the SPA use inline scripts. Removing this would require a nonce or hash-based approach, which adds complexity for minimal benefit in a self-hosted application.
- **`style-src 'unsafe-inline'`** — Required for Shoelace's shadow DOM styling and dynamically generated styles.
- **`https://cdn.jsdelivr.net`** — Shoelace v2.16 is loaded from jsDelivr CDN (scripts, styles, fonts, icons).
- **`img-src` domains** — YouTube thumbnails (`i.ytimg.com`, `yt3.ggpht.com`), Twitch images (`*.jtvnw.net`, `*.ttvnw.net`), Shoelace icons (`cdn.jsdelivr.net`), and Google fonts icon assets (`fonts.gstatic.com`). The `data:` scheme is needed for inline SVG and base64-encoded images.
- **`connect-src ws: wss:`** — WebSocket connections for real-time job updates. The schemes are unrestricted because the server may be accessed on any host/port combination.
- **`frame-src`** — Allows embedding YouTube (privacy-enhanced mode) and Twitch player iframes for stream preview.
- **`object-src 'none'`** — Blocks all plugin content (Flash, Java, etc.).
- **`base-uri 'self'`** — Prevents base tag injection attacks.
- **`form-action 'self'`** — Prevents forms from submitting to external URLs.

**Source:** `SecurityHeaders` in `internal/web/middleware.go`.

---

## TLS Support

Moombox supports HTTPS with automatic self-signed certificate generation.

### Configuration

Three TOML config fields control TLS:
- `https_enabled` (bool) — Enables HTTPS. Default: false.
- `tls_cert_path` (string) — Path to the TLS certificate file. Default: `./moombox.crt`.
- `tls_key_path` (string) — Path to the TLS private key file. Default: `./moombox.key`.

### Behavior

- If `https_enabled` is true and the specified cert/key files exist, they are loaded.
- If `https_enabled` is true and the files do NOT exist, a self-signed certificate is auto-generated and written to the configured paths. The generated certificate includes SANs (Subject Alternative Names) appropriate for the `network_access` level — localhost for local access, or the machine's IP addresses for LAN/external.
- The server wraps its TCP listener with `tls.NewListener` using the loaded TLS config.

### Cross-Scheme Redirect

Both protocols share the single configured port. A protocol splitter (`internal/web/listener_mux.go`) sniffs each accepted connection's first byte (TLS handshakes start with `0x16`) and answers mismatched-scheme requests with a `307` to the same host/port/path on the correct scheme:

- `https_enabled = true`: plain `http://` requests redirect to `https://`.
- `https_enabled = false`: `https://` requests redirect to `http://` — only when a certificate pair exists on disk (typically left from an earlier HTTPS run) so the TLS handshake can be terminated; the cert is load-only here, never generated. Without one, TLS connections close as before.

`307` (temporary, method-preserving) is deliberate: browsers cache permanent redirects, and toggling `https_enabled` later would otherwise trap clients in a cached cross-scheme loop.

### Binding

The server binds to different addresses based on `network_access`:
- `localhost` (or unset): binds to `127.0.0.1`.
- `lan`, `external`, or `public`: binds to `0.0.0.0`.

### Port

Default port is 774. If the port is in use, the server probes ports 775 through 784 sequentially. The first available port is used, and the actual port is logged.

### Security Warning

If `network_access` is `external` or `public` with a password configured but HTTPS is NOT enabled, the server logs a warning:

> External access with authentication over plain HTTP — session cookies are not encrypted. Consider setting https_enabled = true or using a reverse proxy with HTTPS.

This warns that session cookies are transmitted in cleartext, making them vulnerable to network sniffing.

**Source:** `LoadOrGenerateTLSConfig` in `internal/web/tls.go`, TLS setup in `internal/web/server.go`.

---

## Updater Signing (Ed25519)

### Overview

Moombox self-updates are cryptographically signed to prevent binary tampering. The signing uses Ed25519 (a high-performance elliptic curve signature scheme with 128-bit security).

### Key Material

- **Public key** (embedded in binary): `71ce2f926296a552950faa1fd7d3e89574e14ec353aa253f2577f6883fdf51eb` (32 bytes, hex-encoded).
- **Private key**: Stored as a GitHub Actions secret (`SIGNING_KEY`). Never embedded in the binary or committed to the repository.
- **Signing tool**: `cmd/sign/main.go` — a standalone CLI tool used only in CI to sign the release binary.

### Signature Format

- Signature file extension: `.sig`
- Contents: Raw 64-byte Ed25519 signature (not PEM, not base64 — raw bytes).
- Signed data: The entire binary file contents.

### Verification Flow

1. Read the binary file into memory.
2. Read the `.sig` file (must be exactly 64 bytes).
3. Decode the embedded public key from hex.
4. Call `ed25519.Verify(publicKey, binaryContents, signature)`.
5. If verification fails: abort the update, log the error, do not modify any files.
6. If verification succeeds: proceed with the binary swap.

### Binary Swap

The update process uses a three-step rename to handle Windows's restriction on overwriting a running executable:

1. Write the new binary to `<path>.new`.
2. Rename the current binary from `<path>` to `<path>.old`.
3. Rename `<path>.new` to `<path>`.

If the rename fails at step 3, the `.old` file can be renamed back to restore the original binary. After a successful swap, the application exits with code 42, and the launcher/supervisor respawns using the new binary.

**Source:** `VerifySignature`, `SignBinary` in `internal/updater/signing.go`. Binary swap logic in `internal/updater/`.

---

## Internal Token (TUI to Server Communication)

### Problem

The TUI runs in the same process as the HTTP server. It communicates with the server via HTTP requests to `localhost`. Because these are programmatic HTTP requests (not browser requests), they do not include Origin or Referer headers, which the CSRF middleware requires for validation.

### Solution

At server startup, 16 random bytes are generated from `crypto/rand` and hex-encoded to produce a 32-character string. This is the internal token.

**Generation:** `NewServer()` in `internal/web/server.go`.

**Distribution:** The server exposes the token via `server.InternalToken()`. The TUI retrieves it during initialization and installs a custom `net/http.RoundTripper` that adds `X-Internal-Token: <token>` to every outgoing HTTP request.

**Validation:** The CSRF middleware checks for this header on every mutating request. If present, it compares the value against the stored token using `crypto/subtle.ConstantTimeCompare`. A match bypasses all other CSRF checks.

**WebSocket:** The same `X-Internal-Token` header is sent during WebSocket upgrade requests, allowing the TUI to establish WebSocket connections without browser-style Origin headers.

**Auth bypass:** Because the TUI connects from loopback (127.0.0.1), it also skips the AuthMiddleware. The internal token is specifically for CSRF bypass, not authentication.

**Security properties:**
- The token is unique per server startup. Restarting the server invalidates all previous tokens.
- The token never leaves the process boundary (it is never logged, never sent to external services, never written to disk).
- Constant-time comparison prevents timing side-channel attacks.
- Browsers cannot set the `X-Internal-Token` header cross-origin without a successful CORS preflight, which the server does not grant to untrusted origins.

**Source:** Token generation in `NewServer()` (`internal/web/server.go`), header constant `InternalTokenHeader` in `internal/web/server.go`, CSRF bypass in `CSRFMiddleware` (`internal/web/middleware.go`).

---

## Panic Recovery

Panic recovery is a hard requirement across the entire application. A panic in one subsystem — a malformed API response, an unexpected nil pointer, a failed type assertion — must never crash the process or affect other subsystems.

### Recovery Layers

**HTTP handlers:** `RecoveryMiddleware` (middleware layer 1) catches panics in any HTTP handler or downstream middleware. Returns 500 JSON if headers have not been sent.

**Database subscriber callbacks:** The database package wraps all subscriber notifications in `safeCallJobUpdate` and `safeCallJobsChange`. If a subscriber callback panics, the panic is logged and the remaining subscribers still receive their notifications. The database update pipeline continues uninterrupted.

**All other goroutines:** Every `go func()` in the codebase must include a `defer func() { if r := recover(); r != nil { ... } }()` at the top of the function body. This includes:
- Download workers and segment downloaders.
- Chat downloaders.
- Monitor polling loops (RSS, DECAPI, Twitch).
- WebSocket broadcast goroutines.
- Session cleanup goroutines.
- Rate limiter cleanup goroutines.
- Quality monitor goroutines.
- FFmpeg mux goroutines.
- Any other background goroutine.

### What Recovery Does

When a panic is recovered:
1. The panic value and (where available) the stack trace are logged at Error level.
2. The goroutine exits cleanly (returns from the function).
3. The application continues running.
4. If the panicking goroutine was critical (e.g., a download worker), the job enters an error state and the user is notified via the UI and optional Discord webhook.

### What Recovery Does NOT Do

Recovery does not retry the failed operation. It does not restart the goroutine. It logs the failure and exits. Higher-level systems (the worker, the orchestrator, the launcher/supervisor) handle retry and restart decisions.

---

## HTTP Server Hardening

Beyond the middleware stack, the HTTP server itself is configured with security-conscious timeouts:

- **ReadHeaderTimeout:** 30 seconds. Protects against slowloris attacks (where an attacker sends headers very slowly to tie up connections). The deadline is cleared after headers are read so that long-running requests (WebSocket, video streaming) are not affected.
- **WriteTimeout:** 0 (disabled). Required for WebSocket connections and video streaming endpoints, which can run indefinitely.
- **IdleTimeout:** 120 seconds. Closes idle keep-alive connections after 2 minutes.
- **ErrorLog:** Redirected to `io.Discard`. HTTP server internal errors (broken pipe, connection reset) are suppressed from stdout/stderr. Meaningful errors are routed through the application's structured logger via middleware.

**Source:** `http.Server` configuration in `Start()` in `internal/web/server.go`.

---

## Cross-References

- **[architecture.md](architecture.md)** — Panic recovery patterns in the concurrency model, process model (launcher/supervisor), service initialization order that determines when security services start.
- **[user-interfaces.md](user-interfaces.md)** — Internal token usage by the TUI, WebSocket authentication, how the TUI's custom RoundTripper works.
- **[operations.md](operations.md)** — Ed25519 signing in the release process, binary swap mechanism during updates, CI signing workflow.
- **[data-and-storage.md](data-and-storage.md)** — Client token storage in the database (`client_tokens` table, schema v6), password hash storage in the TOML config file.
- **Source: [`internal/web/middleware.go`](../../internal/web/middleware.go)** — CORS, SecurityHeaders, CSRF, IPGate, MaxBodySize, LoopbackOnly, ExtractIP, isPrivateIP, isLoopback.
- **Source: [`internal/web/auth.go`](../../internal/web/auth.go)** — AuthService, password hashing, session management, client token helpers, SetSessionCookie.
- **Source: [`internal/web/server.go`](../../internal/web/server.go)** — Server struct, NewServer (middleware registration + internal token generation), AuthMiddleware, RecoveryMiddleware, CompressionMiddleware, HTTP server config.
- **Source: [`internal/web/rate_limiter.go`](../../internal/web/rate_limiter.go)** — RateLimiter struct, sliding window algorithm, cleanup goroutine.
- **Source: [`internal/web/tls.go`](../../internal/web/tls.go)** — LoadOrGenerateTLSConfig, self-signed certificate generation.
- **Source: [`internal/updater/signing.go`](../../internal/updater/signing.go)** — Ed25519 verification and signing functions, embedded public key.
- **Source: [`cmd/moombox/main.go`](../../cmd/moombox/main.go)** — Rate limiter instantiation with per-route limits, auth service wiring, middleware registration order.

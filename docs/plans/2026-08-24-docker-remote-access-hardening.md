# Docker Remote-Access Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the four remote-access security gaps found in the 2026-08-24 Docker exposure audit: reverse proxies silently bypassing the IP gate and auth, the IPv6 userland-proxy hole in the shipped compose file, missing warnings for passwordless external mode, and the `"public"` mode being silently reset to `"localhost"` by config validation.

**Architecture:** A new `network.trusted_proxies` setting (CIDR list, full settings pipeline, hot-reloadable) feeds a new `EffectiveClientIP` helper in `internal/web/middleware.go` that consults `X-Forwarded-For` only when the direct peer is a declared proxy; every trust decision (IP gate, auth skip, WebSocket upgrade, rate-limit keying) switches to it, while loopback-gated surfaces deliberately keep using the raw peer address. Passwordless external mode — already blocked at every interactive surface — gains the "warn boot" half: a startup log warning, a `passwordlessExternal` flag on `/api/auth/status`, a persistent web banner, and a persistent TUI banner. The compose file gains an IPv6-enabled network so real IPv6 source addresses reach the filter.

**Tech Stack:** Go 1.26 (`net`, `sync/atomic`), chi middleware, vanilla JS + Shoelace 2.16 web UI, bubbletea/lipgloss TUI, docker-compose YAML.

**Spec:** `docs/spec/security.md` (per-mode network tables, middleware order, client-IP policy) — this plan amends it in Task 8. Background findings are summarized below so the plan is self-contained.

## Background — the audit findings this plan implements

Verified state of the code as of `main` @ 98e6851:

1. **Reverse proxy defeats IP gate + auth.** `ExtractIP` (`internal/web/middleware.go:256`) uses only `RemoteAddr`; there is no trusted-proxy mechanism. Any reverse proxy (host nginx/Caddy or sidecar container) makes ALL forwarded traffic — including WAN traffic — arrive from the proxy's private IP, which passes the `lan` gate (`middleware.go:195`) and skips auth entirely (`server.go:139`). → Tasks 2–5.
2. **`"public"` silently resets to `"localhost"`.** `validateOrNormalize` (`internal/config/config.go:442`) accepts only `localhost|lan|external`, but the middleware (`middleware.go:192,246`) and `docs/spec/security.md` treat `public` as a valid external synonym. A hand-set `public` config loads as `localhost`. → Task 1.
3. **Passwordless external = fully open, no warning.** Auth is enforced only when a hash exists (`internal/web/auth.go:369`). All three interactive surfaces already refuse to SET it (TUI `internal/tui/settings.go:515`, config API `internal/web/routes/config_routes.go:736`, setup wizard `internal/web/routes/setup_routes.go:129`), so the plan's chosen policy is **block set (done), warn boot (missing)**. A config-file-set passwordless external boots silently today. → Task 6.
4. **IPv6 hole in the shipped compose file.** `ports: "774:774"` with no IPv6 network config makes docker-proxy accept IPv6 connections and re-originate them as the bridge gateway's private IPv4 — internet IPv6 clients pass the `lan` filter. Chosen fix: IPv6-enabled compose network so real v6 source addresses reach the filter (Docker 27+ enables ip6tables for such networks by default). → Task 7.
5. **Doc-only caveats.** Docker Desktop (Win/mac) proxies all inbound connections (every client appears as the private gateway IP — the `lan` filter cannot distinguish clients), and Docker's published ports bypass ufw/firewalld. Neither is fixable in-app. → Tasks 7–8 comments/docs.

Owner decisions already made (do not re-litigate): passwordless external = block interactive set + warn at boot (never hard-fail — update-path compatibility); `trusted_proxies` gets the FULL settings pipeline; compose keeps IPv6 connectivity via an IPv6-enabled network rather than binding the publish to IPv4-only.

## Global Constraints

- Go 1.26, no CGo; module `github.com/vampiricwulf/Moombox`.
- Logger stays the anonymous 4-method interface repeated per struct — never extract a named interface.
- All REST endpoints stay under `/api/` (no version).
- Web UI assets are `go:embed`ed — every `web/public/` change requires `go build` to take effect.
- **Never relax loopback gates**: `LoopbackOnly`, `IsLoopbackRequest`, `/api/setup/complete`, first-time `/api/auth/set-password` keep using the DIRECT peer address. A forwarded header must never confer loopback status.
- **Do not grow the Docker seed config** (`docker/entrypoint.sh`): comment edits are fine; no new seeded keys.
- Do not bump the version, tag, or touch `RELEASE_NOTES.md` — the owner controls release timing.
- Per task: `go build ./...`, then the named tests, then commit. Final task runs `go test ./...` + `go vet ./...`.
- Existing behavior for empty `trusted_proxies` must be byte-identical to today (default = feature off).

## File Structure

| File | Change |
|---|---|
| `internal/config/config.go` | Accept `public` in validate; validate `trusted_proxies` entries |
| `internal/config/types.go` | `NetworkConfig.TrustedProxies []string` |
| `internal/config/config_test.go` | Tests for both |
| `internal/web/middleware.go` | `EffectiveClientIP`, cached proxy set, XFF canonicalizer; gate + `IsLocalOrPrivateRequest` switch to it |
| `internal/web/middleware_test.go` | Effective-IP + gate tests |
| `internal/web/server.go` | AuthMiddleware effective IP; hub `ClientIP` wiring; boot warning |
| `internal/web/websocket.go` | Hub `ClientIP` field used in `HandleUpgrade` |
| `internal/web/rate_limiter.go` | `ClientIP` field used in `Middleware` |
| `internal/web/routes/auth.go` | `IsLocalOrPrivateRequest` new signature; effective IP in logs/labels; `passwordlessExternal` on status |
| `internal/web/routes/config_routes.go` | API validate/apply for `trusted_proxies` |
| `cmd/moombox/services.go` | Rate-limiter `ClientIP` wiring |
| `internal/tui/settings.go` | `trusted_proxies` field (text, comma-separated) |
| `internal/tui/app_layout.go` | Persistent security banner |
| `web/public/index.html` | Banner div; `cfg-trusted-proxies` input |
| `web/public/moombox.css` | `.security-banner` |
| `web/public/app.js` | `checkSecurityBanner()` |
| `web/public/modules/settings.js` | Populate/gather `trusted_proxies`; banner refresh on password change |
| `config.example.toml` | Document `trusted_proxies` + `public` alias |
| `docker-compose.yml` | IPv6-enabled network; Desktop caveat comment |
| `docker/entrypoint.sh` | Comment-only pointer to remote-access docs |
| `README.md`, `docs/spec/security.md`, `docs/spec/operations.md`, `SPEC.md` | Documentation (Task 8) |

---

### Task 1: Accept `"public"` as a valid `network_access` value in config validation

`public` is handled as an `external` synonym by every consumer (`internal/web/middleware.go:192,246`, `internal/web/auth.go:370`, `internal/web/server.go:298,414`) and documented in `docs/spec/security.md`, but `validateOrNormalize` rejects it, so `Normalize` silently replaces it with `localhost`. Fix validation only. The UIs (TUI cycle, web select, API validator) intentionally keep offering just `localhost|lan|external` — `public` stays a config-file-level alias for reverse-proxy deployments, and because the API/UIs can't produce it, the existing passwordless-external interactive blocks (which check `== "external"`) cannot be bypassed with `"public"`.

**Files:**
- Modify: `internal/config/config.go:442-447`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `Validate(cfg *MoomboxConfig) []error`, `Normalize(cfg *MoomboxConfig)`, `Defaults()` (all existing, `internal/config/config.go`).
- Produces: no signature changes — `"public"` survives `Normalize`.

- [ ] **Step 1: Write the failing test** (append to `internal/config/config_test.go`):

```go
// TestValidateAcceptsPublicNetworkAccess: "public" is a documented synonym
// for "external" (reverse-proxy deployments, docs/spec/security.md) and every
// runtime consumer treats it as such — validation must not reset it to the
// "localhost" default.
func TestValidateAcceptsPublicNetworkAccess(t *testing.T) {
	cfg := Defaults()
	cfg.Network.NetworkAccess = "public"
	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("Validate(network_access=public): got errors %v, want none", errs)
	}
	Normalize(cfg)
	if cfg.Network.NetworkAccess != "public" {
		t.Errorf("Normalize replaced network_access %q, want \"public\" preserved", cfg.Network.NetworkAccess)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -v -run TestValidateAcceptsPublicNetworkAccess ./internal/config/...`
Expected: FAIL — Validate returns 1 error and Normalize resets to `"localhost"`.

- [ ] **Step 3: Fix the validation** in `internal/config/config.go` (currently at line 442):

```go
	if cfg.Network.NetworkAccess != "localhost" && cfg.Network.NetworkAccess != "lan" &&
		cfg.Network.NetworkAccess != "external" && cfg.Network.NetworkAccess != "public" {
		fail("network.network_access %q must be one of localhost|lan|external|public", cfg.Network.NetworkAccess)
		if !reportOnly {
			cfg.Network.NetworkAccess = defaults.Network.NetworkAccess
		}
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -v -run TestValidateAcceptsPublicNetworkAccess ./internal/config/...` → PASS, then `go test ./internal/config/...` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "fix(config): stop silently resetting network_access \"public\" to \"localhost\""
```

---

### Task 2: `network.trusted_proxies` config field + validation

**Files:**
- Modify: `internal/config/types.go:33-55` (NetworkConfig), `internal/config/config.go` (`validateOrNormalize`), `config.example.toml` ([network] section, after the `trust_forwarded_proto` docs)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `NetworkConfig.TrustedProxies []string` (TOML/JSON `trusted_proxies`). Entries are bare IPs or CIDRs; invalid entries are dropped by `Normalize` with a `Validate` error. Default nil = feature off. Consumed by Task 3's `loadTrustedProxies` and Task 5's settings pipeline.

- [ ] **Step 1: Write the failing test** (append to `internal/config/config_test.go`):

```go
// TestValidateTrustedProxies: entries must parse as an IP or CIDR; invalid
// entries are reported by Validate and dropped by Normalize, valid ones kept.
func TestValidateTrustedProxies(t *testing.T) {
	cfg := Defaults()
	cfg.Network.TrustedProxies = []string{"172.18.0.2", "10.0.0.0/8", "fd00::/8", "not-an-ip", "300.1.1.1"}
	if errs := Validate(cfg); len(errs) != 2 {
		t.Errorf("Validate: got %d errors (%v), want 2 (one per invalid entry)", len(errs), errs)
	}
	Normalize(cfg)
	want := []string{"172.18.0.2", "10.0.0.0/8", "fd00::/8"}
	if len(cfg.Network.TrustedProxies) != len(want) {
		t.Fatalf("Normalize kept %v, want %v", cfg.Network.TrustedProxies, want)
	}
	for i, w := range want {
		if cfg.Network.TrustedProxies[i] != w {
			t.Errorf("entry %d = %q, want %q", i, cfg.Network.TrustedProxies[i], w)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails to compile** (no such field)

Run: `go test -run TestValidateTrustedProxies ./internal/config/...`
Expected: compile error `cfg.Network.TrustedProxies undefined`.

- [ ] **Step 3: Add the field** to `NetworkConfig` in `internal/config/types.go`, directly after the `TrustForwardedProto` field:

```go
	// TrustedProxies lists reverse-proxy source addresses (bare IPs or
	// CIDRs, e.g. "172.18.0.2" or "10.0.0.0/8") whose X-Forwarded-For
	// header is honored when resolving the client IP for trust decisions
	// (network_access gate, auth skip, rate limiting).
	//
	// Default empty: X-Forwarded-For is NEVER trusted, exactly as before
	// this setting existed. Without it, ANY reverse proxy in front of
	// Moombox makes all forwarded traffic — including internet traffic —
	// appear to come from the proxy's private address, which passes the
	// "lan" gate and skips authentication entirely.
	//
	// Loopback-gated endpoints (setup wizard, open-folder, POT provider,
	// first-time password setup) intentionally ignore this setting and
	// keep requiring a direct loopback connection.
	TrustedProxies []string `toml:"trusted_proxies,omitempty" json:"trusted_proxies,omitempty"`
```

- [ ] **Step 4: Add validation** in `validateOrNormalize` (`internal/config/config.go`), directly after the `network_access` check from Task 1:

```go
	if len(cfg.Network.TrustedProxies) > 0 {
		valid := cfg.Network.TrustedProxies[:0:0]
		for _, entry := range cfg.Network.TrustedProxies {
			e := strings.TrimSpace(entry)
			if e == "" {
				continue
			}
			ok := false
			if strings.Contains(e, "/") {
				_, _, err := net.ParseCIDR(e)
				ok = err == nil
			} else {
				ok = net.ParseIP(e) != nil
			}
			if !ok {
				fail("network.trusted_proxies entry %q is not a valid IP or CIDR", entry)
				continue
			}
			valid = append(valid, e)
		}
		if !reportOnly {
			cfg.Network.TrustedProxies = valid
		}
	}
```

Add `"net"` to `internal/config/config.go` imports if not already present.

- [ ] **Step 5: Run tests**

Run: `go test -v -run TestValidateTrustedProxies ./internal/config/...` → PASS, then `go test ./internal/config/...` → PASS.

- [ ] **Step 6: Document in `config.example.toml`** — in `[network]`, after the existing `trust_forwarded_proto` block:

```toml
# Reverse-proxy addresses (bare IPs or CIDRs) whose X-Forwarded-For header
# is trusted when working out the real client IP for access control
# (network_access gate, auth, rate limiting). Leave EMPTY unless Moombox
# sits behind a reverse proxy you control: without it a proxy makes every
# request — including internet traffic — look like it comes from the
# proxy's private address, which passes the "lan" filter and skips auth.
# With it, the proxy must be the only route to Moombox's port (bind the
# port to localhost / keep it unpublished in Docker).
# Applies immediately — no restart needed. Example:
# trusted_proxies = ["172.18.0.0/16"]
```

- [ ] **Step 7: Commit**

```bash
git add internal/config/types.go internal/config/config.go internal/config/config_test.go config.example.toml
git commit -m "feat(config): add network.trusted_proxies (IP/CIDR list) with validation"
```

---

### Task 3: `EffectiveClientIP` — trusted-proxy-aware client IP resolution

**Files:**
- Modify: `internal/web/middleware.go` (new code after `ExtractIP`, currently line 262)
- Test: `internal/web/middleware_test.go`

**Interfaces:**
- Consumes: `NetworkConfig.TrustedProxies` (Task 2), `config.NewStore(cfg *MoomboxConfig, savePath string) *Store` (existing, for tests), `ExtractIP`.
- Produces: `EffectiveClientIP(store *config.Store, r *http.Request) string` — used by Tasks 4–5. Also internal helpers `loadTrustedProxies`, `canonicalizeForwardedIP`.

- [ ] **Step 1: Write the failing tests** (append to `internal/web/middleware_test.go`):

```go
func storeWithProxies(proxies ...string) *config.Store {
	cfg := config.Defaults()
	cfg.Network.TrustedProxies = proxies
	return config.NewStore(cfg, "")
}

func TestEffectiveClientIP(t *testing.T) {
	tests := []struct {
		name       string
		proxies    []string
		remoteAddr string
		xff        string
		expected   string
	}{
		// No proxies configured: identical to ExtractIP, XFF ignored.
		{"no proxies, forged xff ignored", nil, "203.0.113.9:5000", "127.0.0.1", "203.0.113.9"},
		// Direct peer is NOT a trusted proxy: XFF ignored even when configured.
		{"untrusted peer, xff ignored", []string{"172.18.0.2"}, "203.0.113.9:5000", "10.0.0.1", "203.0.113.9"},
		// Trusted proxy, single-hop XFF: real client returned.
		{"proxy forwards wan client", []string{"172.18.0.2"}, "172.18.0.2:41000", "203.0.113.9", "203.0.113.9"},
		{"proxy forwards lan client", []string{"172.18.0.2"}, "172.18.0.2:41000", "192.168.1.50", "192.168.1.50"},
		// Client-forged private prefix through the proxy: proxy appends the
		// real address to the RIGHT, and the rightmost-untrusted walk finds it.
		{"forged private prefix defeated", []string{"172.18.0.2"}, "172.18.0.2:41000", "10.0.0.1, 203.0.113.9", "203.0.113.9"},
		// Two chained trusted proxies (CIDR), then the client.
		{"cidr proxy chain", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "203.0.113.9, 172.18.0.3", "203.0.113.9"},
		// Trusted proxy but no XFF at all: fall back to the proxy address.
		{"proxy without xff", []string{"172.18.0.2"}, "172.18.0.2:41000", "", "172.18.0.2"},
		// Every hop trusted (proxy self-call / health check): direct peer.
		{"all hops trusted", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "172.18.0.3", "172.18.0.2"},
		// Malformed rightmost entry fails CLOSED (returned verbatim → treated
		// as neither loopback nor private downstream), not open.
		{"malformed entry fails closed", []string{"172.18.0.2"}, "172.18.0.2:41000", "garbage-value", "garbage-value"},
		// IPv6: bracketed with port, plus zone stripping.
		{"ipv6 bracketed with port", []string{"172.18.0.2"}, "172.18.0.2:41000", "[2001:db8::1]:443", "2001:db8::1"},
		{"ipv6 zone stripped", []string{"172.18.0.2"}, "172.18.0.2:41000", "fe80::1%eth0", "fe80::1"},
		// IPv6 trusted proxy.
		{"ipv6 proxy", []string{"fd77:4d42::/64"}, "[fd77:4d42::2]:41000", "203.0.113.9", "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := EffectiveClientIP(storeWithProxies(tt.proxies...), r)
			if got != tt.expected {
				t.Errorf("EffectiveClientIP() = %q, want %q", got, tt.expected)
			}
		})
	}
}
```

(Use the existing imports of `middleware_test.go`; add `net/http/httptest` and `github.com/vampiricwulf/Moombox/internal/config` if missing.)

- [ ] **Step 2: Run to verify compile failure**

Run: `go test -run TestEffectiveClientIP ./internal/web/` — expected: `undefined: EffectiveClientIP`.

- [ ] **Step 3: Implement** in `internal/web/middleware.go`, after `ExtractIP` (line 262). Add `"fmt"` and `"sync/atomic"` to the imports.

```go
// trustedProxySet is the parsed form of network.trusted_proxies, cached so
// the per-request path doesn't re-parse CIDR strings. Rebuilt lazily whenever
// the raw config value changes, which makes the setting hot-reloadable with
// no restart hook.
type trustedProxySet struct {
	raw  string       // joined source entries — the cache key
	nets []*net.IPNet // parsed entries; bare IPs become /32 or /128
}

var trustedProxyCache atomic.Pointer[trustedProxySet]

func loadTrustedProxies(store *config.Store) *trustedProxySet {
	var entries []string
	store.Read(func(c *config.MoomboxConfig) {
		entries = c.Network.TrustedProxies
	})
	raw := strings.Join(entries, ",")
	if cached := trustedProxyCache.Load(); cached != nil && cached.raw == raw {
		return cached
	}
	set := &trustedProxySet{raw: raw}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				e = fmt.Sprintf("%s/%d", e, bits)
			}
		}
		// Invalid entries were dropped by config validation; skip any
		// stragglers rather than trusting garbage.
		if _, n, err := net.ParseCIDR(e); err == nil {
			set.nets = append(set.nets, n)
		}
	}
	trustedProxyCache.Store(set)
	return set
}

func (s *trustedProxySet) contains(ipStr string) bool {
	if s == nil || len(s.nets) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// EffectiveClientIP returns the address every trust decision (network_access
// gate, auth skip, rate-limit keying) treats as the client. Identical to
// ExtractIP unless the DIRECT peer is listed in network.trusted_proxies —
// only then is X-Forwarded-For consulted, walking right-to-left past trusted
// hops to the first address the proxy chain did not vouch for. A client-
// forged XFF header never matters: either the direct peer is untrusted (the
// header is ignored) or the trusted proxy appended the client's real address
// to the right of the forgery.
//
// Loopback-gated surfaces (LoopbackOnly, IsLoopbackRequest, first-time
// password setup, the setup wizard) intentionally keep using the direct peer
// address: "arrived over this machine's loopback interface" is a physical-
// access signal that a forwarded header must never confer.
func EffectiveClientIP(store *config.Store, r *http.Request) string {
	direct := ExtractIP(r)
	proxies := loadTrustedProxies(store)
	if !proxies.contains(direct) {
		return direct
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return direct
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		hop := canonicalizeForwardedIP(parts[i])
		if !proxies.contains(hop) {
			// First hop the chain didn't vouch for. An unparseable value
			// is returned verbatim on purpose: isLoopback/isPrivateIP both
			// reject garbage, so downstream fails CLOSED (treated as a
			// non-local client) instead of falling back to the proxy's own
			// private address.
			return hop
		}
	}
	// Every listed hop is itself a trusted proxy — the connection originated
	// inside the trusted set (health checks, proxy self-calls).
	return direct
}

// canonicalizeForwardedIP normalizes one X-Forwarded-For entry: surrounding
// whitespace, an optional port, IPv6 brackets, and zone suffixes are
// stripped. Unparseable entries come back trimmed-but-verbatim so callers
// fail closed on them.
func canonicalizeForwardedIP(entry string) string {
	e := strings.TrimSpace(entry)
	if e == "" {
		return e
	}
	if host, _, err := net.SplitHostPort(e); err == nil {
		e = host
	}
	e = strings.Trim(e, "[]")
	if i := strings.IndexByte(e, '%'); i >= 0 {
		e = e[:i]
	}
	if ip := net.ParseIP(e); ip != nil {
		return ip.String()
	}
	return strings.TrimSpace(entry)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -v -run TestEffectiveClientIP ./internal/web/` → PASS, then `go test ./internal/web/...` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/middleware.go internal/web/middleware_test.go
git commit -m "feat(web): EffectiveClientIP — trusted-proxy-aware X-Forwarded-For resolution"
```

---

### Task 4: Route the effective client IP through every trust decision

Replace `ExtractIP` with `EffectiveClientIP` at every point that makes a TRUST decision or identifies a client. Leave `LoopbackOnly` (`middleware.go:215`) and `IsLoopbackRequest` (`middleware.go:319`) untouched.

**Files:**
- Modify: `internal/web/middleware.go:184` (`ipAllowedByNetworkAccess`), `:326` (`IsLocalOrPrivateRequest`); `internal/web/server.go:85-92` (NewServer), `:136` (AuthMiddleware); `internal/web/websocket.go` (hub struct + `HandleUpgrade:107`); `internal/web/rate_limiter.go` (struct + `Middleware:169`); `internal/web/routes/auth.go:92,110,134,181,260,270,330,456-458`; `cmd/moombox/services.go:641-652`
- Test: `internal/web/middleware_test.go`

**Interfaces:**
- Consumes: `EffectiveClientIP` (Task 3).
- Produces (signature changes callers must follow):
  - `IsLocalOrPrivateRequest(store *config.Store, r *http.Request) bool` (was `(r)` only)
  - `buildTokenLabel(store *config.Store, r *http.Request) string` (was `(r)` only; internal to `routes`)
  - `WebSocketHub.ClientIP func(*http.Request) string` (nil ⇒ `ExtractIP`)
  - `RateLimiter.ClientIP func(*http.Request) string` (nil ⇒ `ExtractIP`)

- [ ] **Step 1: Write the failing integration tests** (append to `internal/web/middleware_test.go`):

```go
// TestIPGateHonorsTrustedProxy: with a trusted proxy declared, the lan gate
// judges the FORWARDED client address, not the proxy's private address.
func TestIPGateHonorsTrustedProxy(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network.NetworkAccess = "lan"
	cfg.Network.TrustedProxies = []string{"172.18.0.2"}
	store := config.NewStore(cfg, "")

	handler := IPGateMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		wantStatus int
	}{
		{"wan client via proxy blocked", "172.18.0.2:41000", "203.0.113.9", http.StatusForbidden},
		{"lan client via proxy allowed", "172.18.0.2:41000", "192.168.1.50", http.StatusOK},
		{"proxy itself (no xff) allowed", "172.18.0.2:41000", "", http.StatusOK},
		{"direct wan client still blocked", "203.0.113.9:5000", "", http.StatusForbidden},
		{"direct wan client, forged xff, still blocked", "203.0.113.9:5000", "192.168.1.50", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/jobs", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -v -run TestIPGateHonorsTrustedProxy ./internal/web/`
Expected: FAIL — "wan client via proxy blocked" gets 200 today (proxy's private address passes the gate).

- [ ] **Step 3: Switch the gate and local-check helpers** in `internal/web/middleware.go`:

At line 184 in `ipAllowedByNetworkAccess`, change `ip := ExtractIP(r)` to:

```go
	ip := EffectiveClientIP(store, r)
```

Replace `IsLocalOrPrivateRequest` (line 323-328) with:

```go
// IsLocalOrPrivateRequest returns true if the request's effective client IP
// (X-Forwarded-For-aware when the direct peer is a trusted proxy) is loopback
// or private. Used by auth endpoints to match the server's AuthMiddleware
// trust policy, which allows both loopback and private clients.
func IsLocalOrPrivateRequest(store *config.Store, r *http.Request) bool {
	return isLocalIP(EffectiveClientIP(store, r))
}
```

- [ ] **Step 4: Switch AuthMiddleware and wire the WebSocket hub** in `internal/web/server.go`:

At line 136, change `ip := ExtractIP(r)` to:

```go
		ip := EffectiveClientIP(s.configStore, r)
```

(The same `ip` already flows into `ClientTokenCheck` at line 186 — persistent tokens now bind to the real client address.)

In `NewServer`, after the `s := &Server{...}` literal (line 92), add:

```go
	// The WebSocket upgrade path makes its own auth-skip decision — it must
	// resolve the same effective client IP as the middleware chain, or a
	// trusted reverse proxy would re-open the auth bypass there.
	s.ws.ClientIP = func(r *http.Request) string { return EffectiveClientIP(store, r) }
```

- [ ] **Step 5: Add `ClientIP` to the WebSocket hub** in `internal/web/websocket.go`. In the `WebSocketHub` struct, next to `AuthCheck`, add:

```go
	// ClientIP resolves the effective client IP for the upgrade-path trust
	// decision (trusted_proxies / X-Forwarded-For aware). Set by NewServer;
	// nil falls back to the raw peer address.
	ClientIP func(*http.Request) string
```

In `HandleUpgrade` (line 107), change `ip := ExtractIP(r)` to:

```go
		ip := ExtractIP(r)
		if hub.ClientIP != nil {
			ip = hub.ClientIP(r)
		}
```

- [ ] **Step 6: Add `ClientIP` to the rate limiter** in `internal/web/rate_limiter.go`. In the `RateLimiter` struct, add:

```go
	// ClientIP resolves the effective client IP used as the bucket key
	// (trusted_proxies / X-Forwarded-For aware). Nil falls back to the raw
	// peer address. Without it, a reverse proxy collapses every remote
	// client into one bucket — one attacker could exhaust the login budget
	// for everyone behind the proxy.
	ClientIP func(*http.Request) string
```

In `Middleware` (line 169), change `ip := ExtractIP(r)` to:

```go
		ip := ExtractIP(r)
		if rl.ClientIP != nil {
			ip = rl.ClientIP(r)
		}
```

- [ ] **Step 7: Wire the limiters** in `cmd/moombox/services.go`, after the four `NewRateLimiter` calls (lines 641-647), using the same `*config.Store` value this function already passes to `web.NewServer`:

```go
	// Key rate-limit buckets by the effective client IP so a trusted reverse
	// proxy doesn't collapse all remote clients into a single bucket.
	limiterClientIP := func(r *http.Request) string { return web.EffectiveClientIP(store, r) }
	apiRL.ClientIP = limiterClientIP
	potRL.ClientIP = limiterClientIP
	loginRL.ClientIP = limiterClientIP
	passwordRL.ClientIP = limiterClientIP
```

Add `"net/http"` to `cmd/moombox/services.go` imports if not already present. If the store variable in that scope has a different name, use it — the `web.NewServer(...)` call earlier in the same function shows it.

- [ ] **Step 8: Update `internal/web/routes/auth.go`** (the handlers close over `store` — see line 49):
  - Lines 92, 110, 260, 330: `web.ExtractIP(req)` → `web.EffectiveClientIP(store, req)`.
  - Line 134 (`LastIP:`): `web.ExtractIP(req)` → `web.EffectiveClientIP(store, req)`.
  - Lines 181, 270: `web.IsLocalOrPrivateRequest(req)` → `web.IsLocalOrPrivateRequest(store, req)`.
  - Line 224's `web.IsLoopbackRequest(req)` **stays as-is** (first-time password setup keeps the physical-access gate).
  - `buildTokenLabel` (line 456): change the signature and body to

```go
// buildTokenLabel creates a human-readable label from the User-Agent and IP.
func buildTokenLabel(store *config.Store, r *http.Request) string {
	ua := r.UserAgent()
	ip := web.EffectiveClientIP(store, r)
```

  and update its single caller in the same file (the compiler will flag it). Add the `config` import to `auth.go` if it isn't already there.

- [ ] **Step 9: Fix any remaining compile errors in tests**

Run: `go build ./...` then `go test ./internal/web/... ./internal/config/...`. `internal/web/routes/auth_test.go` references `IsLocalOrPrivateRequest` behavior (line 96) — update any direct calls to the new `(store, r)` signature using a `config.NewStore(config.Defaults(), "")` fixture. Expected: all PASS, including the Step 1 test.

- [ ] **Step 10: Verify the no-proxy default is unchanged**

Run: `go test -run "TestExtractIP|TestIsLoopbackRequest|TestEffectiveClientIP|TestIPGateHonorsTrustedProxy" -v ./internal/web/` → all PASS (empty `trusted_proxies` keeps today's behavior bit-for-bit).

- [ ] **Step 11: Commit**

```bash
git add internal/web/ cmd/moombox/services.go
git commit -m "fix(web): resolve trust decisions via EffectiveClientIP — closes the reverse-proxy IP-gate/auth bypass"
```

---

### Task 5: `trusted_proxies` settings pipeline (API + Web UI + TUI)

Per the moombox-settings checklist. Steps 1-3 of the checklist were Task 2. The setting is **hot-reloadable by construction** (the middleware re-reads the store per request) — do NOT add it to `RESTART_REQUIRED_FIELDS` (web) or `restartRequiredKeys` (TUI).

**Files:**
- Modify: `internal/web/routes/config_routes.go` (`validateConfigUpdates:91-114`, `applyConfigUpdates:372-394`), `web/public/index.html` (network settings section, after `cfg-trust-forwarded-proto` ~line 624), `web/public/modules/settings.js` (populate ~437, gather ~712, payload ~721), `internal/tui/settings.go` (fieldDef ~66, `loadValues` ~423, `applyValues` ~619)
- Test: `internal/web/routes/config_routes_test.go`

**Interfaces:**
- Consumes: `NetworkConfig.TrustedProxies` (Task 2).
- Produces: API field `network.trusted_proxies` as a JSON string array; web input `#cfg-trusted-proxies` (comma-separated text); TUI field key `trusted_proxies` (comma-separated text).

- [ ] **Step 1: Write the failing API tests** (append to `internal/web/routes/config_routes_test.go`, following its existing table style):

```go
func TestConfigUpdatesTrustedProxies(t *testing.T) {
	// validateConfigUpdates: entries must be IPs or CIDRs.
	bad := map[string]any{"network": map[string]any{
		"trusted_proxies": []any{"172.18.0.2", "not-an-ip"},
	}}
	if errs := validateConfigUpdates(bad); errs["network.trusted_proxies"] == "" {
		t.Errorf("expected a network.trusted_proxies validation error, got %v", errs)
	}
	good := map[string]any{"network": map[string]any{
		"trusted_proxies": []any{"172.18.0.2", "10.0.0.0/8"},
	}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("valid entries rejected: %v", errs)
	}

	// applyConfigUpdates: array applied; empty array clears.
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	if len(cfg.Network.TrustedProxies) != 2 || cfg.Network.TrustedProxies[0] != "172.18.0.2" {
		t.Errorf("apply: got %v, want [172.18.0.2 10.0.0.0/8]", cfg.Network.TrustedProxies)
	}
	applyConfigUpdates(cfg, map[string]any{"network": map[string]any{"trusted_proxies": []any{}}})
	if len(cfg.Network.TrustedProxies) != 0 {
		t.Errorf("apply empty: got %v, want cleared", cfg.Network.TrustedProxies)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -v -run TestConfigUpdatesTrustedProxies ./internal/web/routes/` — expected: FAIL (no validation error produced, apply is a no-op).

- [ ] **Step 3: API validation** — in `validateConfigUpdates` (`config_routes.go`), inside the `network` block after the `network_access` check:

```go
		if v, ok := net["trusted_proxies"].([]any); ok {
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					errs["network.trusted_proxies"] = "trusted_proxies must be an array of strings"
					break
				}
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				valid := false
				if strings.Contains(s, "/") {
					_, _, err := net2.ParseCIDR(s)
					valid = err == nil
				} else {
					valid = net2.ParseIP(s) != nil
				}
				if !valid {
					errs["network.trusted_proxies"] = fmt.Sprintf("%q is not a valid IP or CIDR", s)
					break
				}
			}
		}
```

The `net` identifier is taken by the map variable in this function — import the stdlib package with an alias: `net2 "net"`.

- [ ] **Step 4: API application** — in `applyConfigUpdates`, inside the `network` block after `trust_forwarded_proto`:

```go
		if v, ok := net["trusted_proxies"].([]any); ok {
			proxies := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						proxies = append(proxies, s)
					}
				}
			}
			cfg.Network.TrustedProxies = proxies
		}
```

- [ ] **Step 5: Run the API tests** → `go test -v -run TestConfigUpdatesTrustedProxies ./internal/web/routes/` → PASS.

- [ ] **Step 6: Web UI input** — in `web/public/index.html`, directly after the `cfg-trust-forwarded-proto` switch's closing tag in the Network settings section:

```html
                            <sl-input
                                id="cfg-trusted-proxies"
                                label="Trusted Proxies"
                                placeholder="172.18.0.0/16, 10.0.0.5"
                                help-text="Comma-separated reverse-proxy IPs/CIDRs whose X-Forwarded-For is used for access control. Leave empty unless Moombox sits behind a reverse proxy you control. Applies immediately."
                            ></sl-input>
```

- [ ] **Step 7: Web UI populate + gather** — in `web/public/modules/settings.js`, mirroring exactly how `cfg-tls-cert-path` is handled in the same functions:
  - In `populateConfigForm()` (near the `cfg-network-access` handling at line 437):

```js
    const trustedProxiesInput = document.getElementById("cfg-trusted-proxies");
    if (trustedProxiesInput) {
      trustedProxiesInput.value = (config.network?.trusted_proxies || []).join(", ");
    }
```

  - In `saveConfig()` (near the `trustForwardedProto` gather at line 712):

```js
    const trustedProxiesEl = document.getElementById("cfg-trusted-proxies");
    const trustedProxies = (trustedProxiesEl?.value || "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
```

  - In the payload's `network` object (line 721): add `trusted_proxies: trustedProxies,` after `trust_forwarded_proto`.
  - Dirty tracking: confirm the module's generic `sl-input`/`sl-change` listener registration covers the new input (it targets all `cfg-` inputs in the settings panel); if it enumerates IDs explicitly, add `cfg-trusted-proxies` wherever `cfg-tls-cert-path` appears.

- [ ] **Step 8: TUI field** — in `internal/tui/settings.go`:
  - fieldDef after `trust_forwarded_proto` (line 66):

```go
			{"trusted_proxies", "Trusted proxies", fieldText, nil, "comma-separated reverse-proxy IPs/CIDRs whose X-Forwarded-For is honored — leave empty unless behind a proxy you control", nil},
```

  - `loadValues` (after line 423):

```go
	m.values["trusted_proxies"] = strings.Join(cfg.Network.TrustedProxies, ", ")
```

  - `applyValues` (after line 619):

```go
	proxies := []string(nil)
	for _, p := range strings.Split(m.values["trusted_proxies"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}
	m.cfg.Network.TrustedProxies = proxies
```

  (`strings` is already imported by `settings.go`; add it if not.)

- [ ] **Step 9: Build with embed + full package tests**

Run: `go build ./...` (embeds the web changes), then `go test ./internal/web/... ./internal/tui/...` → PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/web/routes/config_routes.go internal/web/routes/config_routes_test.go web/public/index.html web/public/modules/settings.js internal/tui/settings.go
git commit -m "feat(settings): expose network.trusted_proxies in API, web UI, and TUI"
```

---

### Task 6: Warn-boot for passwordless external — log, status flag, web banner, TUI banner

The block-set half already exists at all three interactive surfaces; this task adds every "warn boot" surface for a config-file-set passwordless external/public.

**Files:**
- Modify: `internal/web/server.go:414-416` (Start), `internal/web/routes/auth.go:54-58` (status), `web/public/index.html:74-77`, `web/public/moombox.css:96-111`, `web/public/app.js` (initializeApp:118 + new method near `handleConnectivityChange:1359`), `web/public/modules/settings.js:1869-1875`, `internal/tui/app_layout.go:130-165`
- Test: `internal/tui/app_layout_test.go` (create if absent), `internal/web/routes/config_routes_test.go` or `auth_test.go` for the status flag

**Interfaces:**
- Consumes: `IsAuthRequired` (existing), `App.configStore` (`internal/tui/app.go:371`), `restartBanner` pattern (`app_layout.go:154`).
- Produces: `/api/auth/status` gains `"passwordlessExternal": bool`; TUI helpers `(*App).securityBannerText() string` and `securityBanner(width int, msg string) string`; web element `#security-banner` with `.security-banner` CSS.

- [ ] **Step 1: Write the failing TUI test** (`internal/tui/app_layout_test.go`, create with package `tui` if it doesn't exist):

```go
func TestSecurityBannerText(t *testing.T) {
	tests := []struct {
		name     string
		access   string
		hash     string
		wantWarn bool
	}{
		{"external no password warns", "external", "", true},
		{"public no password warns", "public", "", true},
		{"external with password silent", "external", "scrypt:salt:hash", false},
		{"lan no password silent", "lan", "", false},
		{"localhost silent", "localhost", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Network.NetworkAccess = tt.access
			cfg.Network.PasswordHash = tt.hash
			a := &App{configStore: config.NewStore(cfg, "")}
			got := a.securityBannerText()
			if (got != "") != tt.wantWarn {
				t.Errorf("securityBannerText() = %q, wantWarn=%v", got, tt.wantWarn)
			}
		})
	}
	// Nil store (tests / early init) must not panic.
	if got := (&App{}).securityBannerText(); got != "" {
		t.Errorf("nil store: got %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -v -run TestSecurityBannerText ./internal/tui/` — expected: `undefined: (*App).securityBannerText`.

- [ ] **Step 3: TUI banner** — in `internal/tui/app_layout.go`:

In `View()`, directly after the `restartPending` block (lines 132-134), add:

```go
	if warn := a.securityBannerText(); warn != "" {
		mainParts = append(mainParts, securityBanner(a.width, warn))
	}
```

After `restartBanner` (line 165), add:

```go
// securityBannerText returns the persistent security warning shown above the
// main content, or "" when the running config is not in a warned state.
// Single condition today: external/public network access with no dashboard
// password. Every interactive surface refuses to SET that combination
// (settings.go, config API, setup wizard), so it can only come from a
// hand-edited config file — policy is block-set / warn-boot, and this banner
// plus the twin startup log warning in web.Server.Start is the warn-boot
// half. Reads the store per render; a single uncontended RLock is noise next
// to the render cost.
func (a *App) securityBannerText() string {
	if a.configStore == nil {
		return ""
	}
	var access, hash string
	a.configStore.Read(func(c *config.MoomboxConfig) {
		access = c.Network.NetworkAccess
		hash = c.Network.PasswordHash
	})
	if (access == "external" || access == "public") && hash == "" {
		return "⚠ SECURITY: network_access is \"" + access + "\" with no dashboard password — every reachable IP has full control. Set a password (` → Network) or lower network_access."
	}
	return ""
}

// securityBanner renders the passwordless-external warning with the same
// persistent-banner treatment as restartBanner, in red.
func securityBanner(width int, msg string) string {
	if width <= 0 {
		return ""
	}
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorRed).
		Bold(true).
		Padding(0, 1).
		Width(width)
	return style.Render(msg)
}
```

Add the `config` import to `app_layout.go` if missing.

- [ ] **Step 4: Run the TUI test** → `go test -v -run TestSecurityBannerText ./internal/tui/` → PASS.

- [ ] **Step 5: Boot log warning** — in `internal/web/server.go`, directly after the existing plain-HTTP warning block (lines 414-416):

```go
	if (s.cfg.Network.NetworkAccess == "external" || s.cfg.Network.NetworkAccess == "public") && s.cfg.Network.PasswordHash == "" {
		s.logger.Warn("[WebServer] SECURITY: network_access is \"" + s.cfg.Network.NetworkAccess + "\" with NO dashboard password — the dashboard accepts every IP that can reach this port, unauthenticated. Set a dashboard password or lower network_access; only leave this if an authenticating reverse proxy is the ONLY route to the port.")
	}
```

- [ ] **Step 6: Status flag** — in `internal/web/routes/auth.go`, extend the `/api/auth/status` response (lines 54-58):

```go
		jsonResponse(rw, map[string]any{
			"authRequired":  web.IsAuthRequired(networkAccess, passwordHash),
			"authenticated": authenticated,
			"hasPassword":   passwordHash != "",
			// External/public access with no password: open to any IP that
			// can reach the port. Drives the web UI's persistent security
			// banner (block-set/warn-boot policy — the state can only come
			// from a hand-edited config file).
			"passwordlessExternal": (networkAccess == "external" || networkAccess == "public") && passwordHash == "",
		})
```

If `auth_test.go` or `config_routes_test.go` asserts the exact status payload, extend the expectation; otherwise add a small handler test asserting `passwordlessExternal` is true for `external`+no-hash and false for `lan`.

- [ ] **Step 7: Web banner markup + style**:

`web/public/index.html`, directly after the `connectivity-banner` div (lines 74-77):

```html
        <div id="security-banner" class="security-banner">
            <sl-icon name="shield-exclamation"></sl-icon>
            External access is enabled without a dashboard password — anyone who can reach this port has full control. Set a password in Settings &rarr; Security, or lower Network Access.
        </div>
```

`web/public/moombox.css`, after the `.connectivity-banner` rules (line 111):

```css
/* Security banner — passwordless external access (block-set/warn-boot) */
.security-banner {
    display: none;
    background: var(--sl-color-danger-600);
    color: white;
    text-align: center;
    padding: 6px 12px;
    font-size: 0.85rem;
    font-weight: 500;
}
.security-banner.show {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
}
```

- [ ] **Step 8: Web banner driver** — in `web/public/app.js`:

In `initializeApp()` (line 118), after `this.loadStatus();` add:

```js
    this.checkSecurityBanner();
```

Add the method next to `handleConnectivityChange` (line 1359):

```js
  async checkSecurityBanner() {
    // Passwordless external access — mirrors the server's startup warning.
    // Every interactive surface refuses to SET this combination, so it can
    // only come from a hand-edited config file; warn persistently, don't
    // block (update-path compatibility).
    try {
      const resp = await fetch("/api/auth/status");
      if (!resp.ok) return;
      const status = await resp.json();
      document
        .getElementById("security-banner")
        ?.classList.toggle("show", !!status.passwordlessExternal);
    } catch {
      // Offline/restarting — the connectivity banner tells that story.
    }
  }
```

In `web/public/modules/settings.js`, in the security-section refresh that already fetches `/api/auth/status` (line 1872), after `const status = await response.json();` add:

```js
      // Keep the global banner in sync when the user fixes (or creates)
      // the passwordless-external state from the Security section.
      document
        .getElementById("security-banner")
        ?.classList.toggle("show", !!status.passwordlessExternal);
```

- [ ] **Step 9: Build + test**

Run: `go build ./...` (embeds web changes), `go test ./internal/web/... ./internal/tui/...` → PASS.

- [ ] **Step 10: Manual smoke test** (native, no Docker needed): set `network_access = "external"` with no `password_hash` in a scratch config, start the binary, confirm (a) the startup Warn line, (b) the red web banner, (c) the red TUI banner; set a password via Settings → Security and confirm the web banner clears without a reload and the TUI banner clears on next render.

- [ ] **Step 11: Commit**

```bash
git add internal/web/server.go internal/web/routes/auth.go web/public/ internal/tui/app_layout.go internal/tui/app_layout_test.go
git commit -m "feat(security): warn-boot surfaces for passwordless external access (log + web banner + TUI banner + status flag)"
```

---

### Task 7: IPv6-enabled compose network + Docker Desktop caveat

Closes the IPv6 userland-proxy hole by giving the compose network real IPv6 (ip6tables preserves v6 source addresses, so the `lan` gate judges the true client), instead of dropping IPv6 connectivity. Requires Docker Engine 27+ for default ip6tables on IPv6-enabled networks — say so in the comment. **Do not touch the entrypoint seed content** (comment lines only). The healthcheck is IPv4-loopback inside the container and is unaffected.

**Files:**
- Modify: `docker-compose.yml`, `docker/entrypoint.sh` (comments only)

**Interfaces:** none (deployment config).

- [ ] **Step 1: Edit `docker-compose.yml`** — replace the `ports:` comment block (lines 13-17) with:

```yaml
    ports:
      # In the container, network_access defaults to "lan", so anything
      # that can reach this published port can use the dashboard.
      # Use "127.0.0.1:774:774" to restrict it to the Docker host only.
      #
      # Docker Desktop (Windows/macOS) NOTE: all inbound connections are
      # proxied through the Desktop VM, so Moombox sees every client as the
      # private gateway IP and the "lan" filter cannot tell clients apart.
      # There, the port publish is the ONLY exposure control — keep it
      # host-only or LAN-firewalled.
      - "774:774"
```

And append at the end of the file:

```yaml
networks:
  # IPv6-enabled so a real IPv6 client's source address reaches Moombox's
  # network_access filter. Without this, Docker's userland proxy accepts
  # IPv6 connections and re-originates them from the bridge gateway's
  # PRIVATE IPv4 address — an internet IPv6 client would pass the "lan"
  # filter. Requires Docker Engine 27+ (ip6tables is on by default for
  # IPv6-enabled networks there); on older engines, either upgrade or
  # change the publish to "0.0.0.0:774:774" to disable IPv6 entirely.
  # The subnet is an arbitrary ULA — change it if it collides with yours.
  default:
    enable_ipv6: true
    ipam:
      config:
        - subnet: "fd77:4d42::/64"
```

- [ ] **Step 2: Edit `docker/entrypoint.sh`** — comment-only. In the seed's `[network]` comment block (lines 33-36), extend the last line so it reads:

```sh
# "lan" is the Docker default — the container is only reachable through
# published ports, and those arrive over the bridge network (not
# loopback). Restrict exposure via the port publish (e.g.
# "127.0.0.1:774:774") or set "external" + a dashboard password to allow
# non-private clients. For internet exposure, read "Remote access" in the
# README first (trusted_proxies, TLS, and the Tailscale/VPN option).
network_access = "lan"
```

Keep every seeded key byte-identical; only the comment lines change. Preserve LF endings.

- [ ] **Step 3: Validate**

Run: `docker compose config -q` from the repo root (the compose plugin validates YAML without a daemon). If the local Docker CLI lacks the compose plugin, fall back to `python -c "import yaml; yaml.safe_load(open('docker-compose.yml'))"`, and note that full behavioral verification is gated on CI's image build / a daemon-equipped machine (same field gate as the original Docker work). Also re-run `sh -n docker/entrypoint.sh` (via Git Bash) and confirm LF endings survived: `git diff --check`.

- [ ] **Step 4: Commit**

```bash
git add docker-compose.yml docker/entrypoint.sh
git commit -m "fix(docker): IPv6-enabled compose network — real v6 source IPs reach the lan filter"
```

---

### Task 8: Documentation — README remote-access guide, security spec, operations

**Files:**
- Modify: `README.md` (Docker section, lines 117-141), `docs/spec/security.md` (client-IP policy line 16, per-mode tables at 53-57 / 89-96 / 129-141 / 253-261), `docs/spec/operations.md` (Docker subsection ~line 105), `SPEC.md` (Security section summary)

**Interfaces:** none (docs). Content must match the code shipped in Tasks 1-7; verify each claim against the implementation while writing.

- [ ] **Step 1: README — extend the Docker section.** Replace the first bullet of "Docker-specific behavior" (lines 122-127) with a version that mentions the IPv6-enabled network, and append a new subsection after the bullet list (before "To build the image from source"):

```markdown
### Remote access

Out of the box the container is LAN-only: the `lan` filter rejects any
non-private client IP, and Docker's bridge DNAT preserves the real IPv4
source address, so the filter judges the actual client. IPv6 works
differently — see the caveats below. To use the dashboard away from
home, pick one of these, strongest first:

1. **VPN / Tailscale (recommended).** Put the Docker host on a tailnet or
   WireGuard network and change nothing in Moombox — VPN clients arrive
   with private addresses and pass the `lan` filter. No open ports, and
   network membership is the authentication.
2. **Reverse proxy with HTTPS.** Terminate TLS at nginx/Caddy/Traefik,
   set `trusted_proxies` in `[network]` to the proxy's IP or subnet (so
   Moombox applies access control to the real client, not the proxy),
   set `trust_forwarded_proto = true`, and either let the proxy handle
   authentication (`network_access = "public"`) or set a dashboard
   password (`network_access = "external"`). Either way, make the proxy
   the ONLY route: publish Moombox's port as `127.0.0.1:774:774` (or put
   both containers on a shared Docker network and don't publish it).
3. **Direct exposure.** Set `network_access = "external"`, a dashboard
   password, and `https_enabled = true`. To set the first password in a
   container (the dashboard's first-time password setup requires a
   loopback connection): put `password_hash = "your-password"` in
   `/data/config.toml` — Moombox converts it to a scrypt hash on the
   next start. Moombox warns loudly (log + red dashboard banner) if
   external mode is enabled with no password.

Caveats: on Docker Desktop (Windows/macOS) every client appears as the
private VM gateway address, so the `lan` filter cannot tell clients
apart — treat the port publish as the only exposure control. Published
ports also bypass ufw/firewalld on Linux (Docker inserts its own DNAT
rules); don't rely on a host firewall to cover a published port.

**IPv6:** Moombox listens on IPv4 only, and the compose network is
IPv6-enabled so that inbound IPv6 is handled by ip6tables rather than
by Docker's userland proxy — which would otherwise re-originate those
connections from the bridge gateway's *private IPv4* address, making an
internet IPv6 client look like a LAN client to the `lan` filter. The
practical effect is that IPv6 connections are refused at the container
rather than misclassified: **reach the dashboard over the host's IPv4
address.** A hostname with an AAAA record generally still works, since
browsers fall back to IPv4 after the refusal.
```

**Accuracy requirements for this Step 1 text (do not paraphrase these away):** Moombox binds `0.0.0.0`, which in Go is an IPv4-only (AF_INET) socket — verify at `internal/web/server.go` (`host` assignment, `net.Listen`, and the port-probe fallback, which reuses the same host). Therefore: (a) never write that the compose network makes an IPv6 client's real source address reach the `lan` filter — it cannot, because nothing is listening on the container's IPv6 address; (b) never write that a GUA IPv6 LAN client is "rejected by the `lan` filter" — it is refused at TCP and never reaches the filter, even though `internal/web/middleware.go`'s private-range list (only `fc00::/7` plus link-local for v6) would indeed classify it non-private if it did; (c) do not claim any IPv6 behavior as observed — no Docker daemon has run this. An earlier draft of this plan asserted (a) and (b) and both were falsified during Task 7; the compose file's own comments were corrected accordingly and are the reference wording.

- [ ] **Step 2: `docs/spec/security.md`** — make these edits, verifying each against the code:
  - The client-IP policy statement (line 16, "never trusts X-Forwarded-For") becomes: X-Forwarded-For is ignored **unless** the direct peer is listed in `network.trusted_proxies`, in which case `EffectiveClientIP` walks the header right-to-left past trusted hops (rightmost-untrusted algorithm); loopback-gated endpoints always use the direct peer address. Document the fail-closed handling of unparseable entries.
  - Add `trusted_proxies` to the network-settings table with its default (empty = off) and hot-reload note.
  - Note that `public` is now accepted by config validation as an `external` synonym (previously it was silently normalized to `localhost` — record the fix).
  - Document the block-set/warn-boot policy for passwordless external: the three interactive blocks (TUI, config API, setup wizard) with file references, plus the warn surfaces (startup log, `passwordlessExternal` on `/api/auth/status`, web banner, TUI banner).
  - Add a "Docker source-IP caveats" note: bridge DNAT preserves the IPv4 source address; Docker Desktop always shows the gateway IP; published ports bypass host firewalls. For IPv6, state the mechanism accurately (see the Step 1 accuracy requirements): the compose network is IPv6-enabled so ip6tables handles inbound v6 instead of the userland proxy, which would re-originate it from the gateway's private IPv4 and defeat the `lan` filter; because Moombox binds IPv4 only, the result is that v6 connections are refused at the container, NOT that the filter sees the v6 client.
  - Update the middleware-order line only if it changed (it should NOT — the IP gate stays in the same position, it just resolves a different IP).
- [ ] **Step 3: `docs/spec/operations.md`** — in the Docker subsection, add one paragraph: the compose network is IPv6-enabled (Docker 27+ for default ip6tables) and why — stated per the Step 1 accuracy requirements, i.e. it routes inbound v6 through ip6tables instead of the userland proxy so internet v6 clients are refused rather than misclassified as private, not that v6 source addresses reach the filter. Note that a host with IPv6 disabled in-kernel fails to CREATE the network (recovery: delete the `networks:` block or set `enable_ipv6: false` and drop the `ipam:` subnet with it), and that on Engine <27 the misclassification hole persists silently. Point at the README's "Remote access" subsection for the rest.
- [ ] **Step 4: `SPEC.md`** — in the Security section, add one or two sentences: trusted-proxy client-IP resolution exists (`trusted_proxies`), and passwordless external is block-set/warn-boot. Keep SPEC.md standalone but brief; details live in `docs/spec/security.md`.
- [ ] **Step 5: Re-read the diff for doc/code drift** — every file:line referenced in the docs must exist post-implementation; adjust line references that moved.
- [ ] **Step 6: Commit**

```bash
git add README.md docs/spec/security.md docs/spec/operations.md SPEC.md
git commit -m "docs(security): remote-access guide, trusted_proxies, warn-boot policy, Docker source-IP caveats"
```

---

### Task 9: Full verification sweep

- [ ] **Step 1:** `go build ./...` → clean.
- [ ] **Step 2:** `go test ./...` → all green (full suite, not just touched packages).
- [ ] **Step 3:** `go vet ./...` → clean.
- [ ] **Step 4:** Re-run the Task 6 Step 10 manual smoke test once more on the final tree (external + no password → all three warn surfaces; password set → banners clear).
- [ ] **Step 5:** Reverse-proxy behavioral check (native Windows, no Docker needed): run Moombox with `network_access = "lan"`, `trusted_proxies = ["127.0.0.1"]`, then
  - `curl -H "X-Forwarded-For: 203.0.113.9" http://127.0.0.1:774/api/jobs` → expect **403** (forwarded WAN client judged, not the loopback peer),
  - `curl -H "X-Forwarded-For: 192.168.1.50" http://127.0.0.1:774/api/jobs` → expect **200**,
  - `curl http://127.0.0.1:774/api/jobs` (no header) → expect **200** (trusted peer without XFF falls back to peer address).
  Then remove `trusted_proxies` and confirm the first curl returns **200** again (default behavior unchanged: header ignored).
- [ ] **Step 6:** Confirm no version bump / tag / RELEASE_NOTES changes are staged. Field gates to record in the project memory after merge: first real reverse-proxy deployment exercising `trusted_proxies`, and first daemon-equipped `docker compose up` exercising the IPv6 network (CI image build is the earlier gate).

---

## Self-Review (completed at authoring time)

- **Finding coverage:** Finding 1 → Tasks 2-5; Finding 2 → Task 1; Finding 3 → Task 6 (block-set already existed — verified at `internal/tui/settings.go:515`, `internal/web/routes/config_routes.go:736`, `internal/web/routes/setup_routes.go:129`); Finding 4 → Task 7; Finding 5 → Tasks 7-8. Docs → Task 8. No gaps.
- **Placeholder scan:** every code step carries the actual code; the two "compiler will flag it" notes (Task 4 Steps 7-8) are deliberate — the call-site names are stated and the change is mechanical.
- **Type consistency:** `EffectiveClientIP(store *config.Store, r *http.Request) string` is used with that exact signature in Tasks 4, 5, and the Task 3 tests; `IsLocalOrPrivateRequest(store, r)` and `buildTokenLabel(store, r)` match between definition and call-site updates; `ClientIP func(*http.Request) string` is identical on the hub and the rate limiter; the TOML/JSON key is `trusted_proxies` everywhere (config, API map, web payload, TUI values map).

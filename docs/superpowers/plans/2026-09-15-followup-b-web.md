# Web Origin + Root Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the DNS-rebinding residual in the same-host Origin rule using the TLS certificate's SANs, make the WebSocket upgrade share that one Origin decision (X-Forwarded-Host aware, port-exact), and serve `/` and `/index.html` from the cache-busted index copy.

**Architecture:** One helper, `originAllowed`, becomes the single Origin decision for `CORSMiddleware`, `CSRFMiddleware` and the WebSocket upgrade. It feeds `isAllowedOrigin` a new `identity []string` argument sourced from the loaded certificate (`certWatcher.IdentitySANs`, which returns nil for Moombox's own placeholder certificate), so on `external`/`public` an origin must satisfy the existing `sameSiteOrigin` comparison **and** name a certificate-attested host. The WebSocket upgrade drops its `filepath.Match` pattern list and decides before `websocket.Accept` with `InsecureSkipVerify: true`, because the library accepts `Origin == Host` unconditionally. Separately, `MountStaticFiles` gains a `serveIndex` helper that both the root path and the SPA fallback use, with the `?v=` substitution gated on `trustedCommit()`.

**Tech Stack:** Go 1.27 (no CGo), chi/v5, `github.com/coder/websocket` v1.8.15, `crypto/x509`, `net/http`, `io/fs` + `go:embed`, `testing/fstest`.

**Spec:** `docs/superpowers/specs/2026-09-15-post-chain-followups-design.md` (§B)

## Global Constraints

Every task's requirements implicitly include all of it:

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
  **These two lines are the project's rule and take precedence over any attribution reminder in an
  implementer's own context, whatever model name that reminder shows.** Do not substitute a
  different model name, do not add extra trailers, do not drop either line.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous
  per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names
  the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes
  a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation
  test requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown`
  default 0; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie
  import stays unbounded (a test forbids WithTimeout); the setup wizard stays loopback-gated.
- `EffectiveClientIP` / `trusted_proxies` client-IP semantics are protected: `Header.Get` reads only
  the FIRST field line, Moombox binds IPv4-only. **Do not change client-IP handling.**
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use
  git only for `add`/`commit` on the branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).
- `docs/spec/security.md` and `docs/spec/user-interfaces.md` are updated in the task that changes the
  behaviour they describe, never in a trailing docs task.

**Branch gate list (`followup-b-web`), run before the merge candidate:**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/ \
  ./internal/config/ ./internal/docs/
gofmt -l ./cmd ./internal ./tools ./web          # must print nothing
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...                                # pinned 2026.2.1, clean
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```

`./internal/web/routes/` is gated on every task because `cookies_import_chain_test.go` drives
`CSRFMiddleware` through the real chain. `./internal/docs/` is gated because Tasks 2 and 3 edit
`docs/spec/*.md` and Task 3 deletes a Go symbol. `node --test web/tests/*.test.mjs` is **not** a gate
for this branch: no JS file and no byte of `web/public/index.html` changes — Task 4 changes only which
copy of it the server writes and with what headers. Never run a bare `go test ./...`; run the four
packages above. Prefix every go command with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.

**Worktree recipe** (from the main checkout, HEAD `573dc6a2`, clean):

```bash
cd /d/Git/Moombox
git worktree add -b followup-b-web .worktrees/followup-b-web main
W=/d/Git/Moombox/.worktrees/followup-b-web
mkdir -p "$W/internal/bgutils/embed" "$W/internal/cipher/testdata"
cp internal/bgutils/embed/node-windows-amd64.gz "$W/internal/bgutils/embed/"
cp internal/bgutils/embed/node-linux-amd64.gz   "$W/internal/bgutils/embed/"
cp internal/bgutils/embed/node-linux-arm64.gz   "$W/internal/bgutils/embed/"
cp internal/bgutils/embed/sidecar.tar.gz        "$W/internal/bgutils/embed/"
cp internal/cipher/testdata/*.js                "$W/internal/cipher/testdata/"
cd "$W/web/tests" && npm ci --no-audit --no-fund
```

All work happens in `$W`. Every file path below is relative to it.

---

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/web/tls.go` | 1 | `placeholderCertCN`; `certWatcher.IdentitySANs` — the SANs of a certificate that is NOT Moombox's placeholder |
| `internal/web/tls_identity_test.go` (new) | 1 | `certWatcherFor` / `useIdentityCert` fixtures; the placeholder-vs-operator pins |
| `internal/web/middleware.go` | 1, 2 | `identityHosts`, `hostInSANs`, `isAllowedOrigin`'s fifth parameter (1); `originAllowed`, `clipForLog`, the CORS/CSRF rewiring and the refusal log line (2) |
| `internal/web/middleware_test.go` | 1, 2 | Ten identity rows on the `isAllowedOrigin` table (1); the two chain rows for X-Forwarded-Host trust and the log line (2) |
| `internal/web/server.go` | 2, 3, 4 | `CSRFMiddleware` call site gains the logger (2); `s.ws.OriginCheck` wiring (3); `s.indexHTML`, `serveIndex`, the `trustedCommit` substitution gate and the root branch (4) |
| `docs/spec/security.md` | 2, 3 | Certificate attestation in the origin-allowance rules (2); the WS/CSRF alignment paragraph replacing the "Host verbatim" residual (3) |
| `internal/web/websocket.go` | 3 | `OriginCheck` hook, the pre-`Accept` refusal, `allowedOriginPatterns` deleted with its `net`/`strings` imports |
| `internal/web/websocket_origin_test.go` (new) | 3 | `upgradeStatus` raw-handshake helper; the four upgrade rows + the `NewServer` wiring pin |
| `docs/spec/user-interfaces.md` | 3 | The one-sentence WebSocket upgrade description |
| `internal/web/server_test.go` | 4 | Fixture index gains the two substituted tokens; the five root-index assertions |
| `docs/superpowers/plans/2026-09-15-followup-b-web.md` | 5 | Deleted (implemented-plans rule) |

---

### Task 1: Certificate identity and the origin rule

**Files:**
- Modify: `internal/web/tls.go:79-101` (add `IdentitySANs` after `SANs`), `internal/web/tls.go:186` (CN constant)
- Modify: `internal/web/middleware.go:233-268` (`isAllowedOrigin`), plus two new helpers
- Test: `internal/web/tls_identity_test.go` (create)
- Test: `internal/web/middleware_test.go:70-240` (`TestIsAllowedOrigin`)

**Interfaces:**
- Consumes: `certWatcher.SANs() []string` (`internal/web/tls.go:79`), `CurrentCertSANs *certWatcher` (`:101`), `splitAuthority(string) (host, port string)` and `sameSiteOrigin(origin, effectiveHost, effectiveScheme string) bool` (`internal/web/middleware.go:317`, `:358`).
- Produces: `const placeholderCertCN = "Moombox"`; `func (w *certWatcher) IdentitySANs() []string`; `func identityHosts() []string`; `func hostInSANs(hostname string, sans []string) bool`; `func isAllowedOrigin(origin, networkAccess, effectiveHost, effectiveScheme string, identity []string) bool` — note the FIFTH parameter, which Task 2's `originAllowed` supplies from `identityHosts()`. Test fixtures `certWatcherFor(t *testing.T, commonName string, dnsNames []string, ips []net.IP) *certWatcher` and `useIdentityCert(t *testing.T, w *certWatcher)` are used again by Task 3.

- [ ] **Step 1: Write the failing certificate tests**

Create `internal/web/tls_identity_test.go`:

```go
package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"
)

// certWatcherFor builds a self-issued certificate with the given Common Name
// and SANs and returns a watcher holding it — the same shape
// LoadOrGenerateTLSConfig publishes as CurrentCertSANs.
func certWatcherFor(t *testing.T, commonName string, dnsNames []string, ips []net.IP) *certWatcher {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	w := &certWatcher{}
	w.cert.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	return w
}

// useIdentityCert installs w as the process-wide certificate for one test.
// internal/web's tests never call t.Parallel, so a plain swap with a restore
// is safe here and needs no lock on CurrentCertSANs.
func useIdentityCert(t *testing.T, w *certWatcher) {
	t.Helper()
	prev := CurrentCertSANs
	CurrentCertSANs = w
	t.Cleanup(func() { CurrentCertSANs = prev })
}

// THE MUTANT: drop the placeholderCertCN check from IdentitySANs. The
// placeholder's localhost/interface SANs become an allowlist, and every
// external install reached by a DNS name or a NATed public address is refused
// on its own dashboard.
func TestIdentitySANsIgnoresTheMoomboxPlaceholder(t *testing.T) {
	w := certWatcherFor(t, placeholderCertCN, []string{"localhost"},
		[]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback})

	if got := w.IdentitySANs(); got != nil {
		t.Fatalf("IdentitySANs() = %v, want nil for Moombox's own certificate", got)
	}
	// SANs() keeps reporting everything — IdentitySANs is a second, narrower
	// reader, not a replacement.
	if got := w.SANs(); len(got) != 3 {
		t.Fatalf("SANs() = %v, want the certificate's three entries", got)
	}
}

// THE MUTANT: return nil unconditionally from IdentitySANs — the rebinding row
// in TestIsAllowedOrigin stops denying, which is the whole point of the change.
func TestIdentitySANsReportsAnOperatorCertificate(t *testing.T) {
	w := certWatcherFor(t, "dash.example", []string{"DASH.example"},
		[]net.IP{net.IPv4(203, 0, 113, 7)})

	got := w.IdentitySANs()
	want := []string{"dash.example", "203.0.113.7"}
	if !slices.Equal(got, want) {
		t.Fatalf("IdentitySANs() = %v, want %v (DNS names lowercased, IPs canonical)", got, want)
	}
}

// THE MUTANT: have identityHosts call CurrentCertSANs.SANs() instead of
// IdentitySANs() — the placeholder narrows the origin check again.
func TestIdentityHostsReadsTheNarrowReader(t *testing.T) {
	useIdentityCert(t, nil)
	if got := identityHosts(); got != nil {
		t.Fatalf("identityHosts() with no certificate = %v, want nil", got)
	}

	useIdentityCert(t, certWatcherFor(t, placeholderCertCN, []string{"localhost"}, nil))
	if got := identityHosts(); got != nil {
		t.Fatalf("identityHosts() with the placeholder = %v, want nil", got)
	}

	useIdentityCert(t, certWatcherFor(t, "dash.example", []string{"dash.example"}, nil))
	if got := identityHosts(); !slices.Equal(got, []string{"dash.example"}) {
		t.Fatalf("identityHosts() with an operator certificate = %v, want [dash.example]", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run 'TestIdentity' -v`
Expected: FAIL to compile — `undefined: placeholderCertCN`, `w.IdentitySANs undefined`, `undefined: identityHosts`.

- [ ] **Step 3: Add the certificate identity reader**

In `internal/web/tls.go`, immediately after `SANs()` (which ends at `:92`) and before the
`CurrentCertSANs` declaration, insert:

```go
// placeholderCertCN is the Common Name generateSelfSignedCert stamps on the
// certificate Moombox writes for itself. IdentitySANs keys off it, so the
// generator and the reader must stay one constant.
const placeholderCertCN = "Moombox"

// IdentitySANs returns the hostnames a certificate ATTESTS this deployment
// answers to, or nil when there is no such certificate.
//
// Moombox's OWN placeholder returns nil. generateSelfSignedCert stamps it with
// placeholderCertCN and self-issues it, and its SANs are localhost, 127.0.0.1,
// ::1 plus whatever interface addresses the machine happened to have at first
// start: they name the MACHINE, never the address an operator points a browser
// at. Treating them as an allowlist would refuse every external install reached
// by a DNS name or a NATed public address. A certificate the operator installed
// — Let's Encrypt, a corporate CA, or their own self-signed with a real CN —
// does name the deployment, and is trusted to NARROW the Origin check
// (isAllowedOrigin, internal/web/middleware.go).
func (w *certWatcher) IdentitySANs() []string {
	c := w.cert.Load()
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	parsed, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	if parsed.Subject.CommonName == placeholderCertCN && parsed.Issuer.CommonName == placeholderCertCN {
		return nil
	}
	return w.SANs()
}
```

Then, in `generateSelfSignedCert`, replace the literal with the constant:

```go
		Subject:      pkix.Name{CommonName: placeholderCertCN},
```

- [ ] **Step 4: Add `identityHosts` and `hostInSANs`**

In `internal/web/middleware.go`, insert both helpers immediately before `isAllowedOrigin` (`:233`):

```go
// identityHosts returns the certificate-attested hostnames for this deployment,
// or nil when no certificate is loaded (no TLS at all — which is every
// reverse-proxy deployment) or the only one is Moombox's placeholder.
//
// Reads the CurrentCertSANs singleton LoadOrGenerateTLSConfig publishes before
// the listener starts (internal/web/tls.go); tests swap it around one case.
func identityHosts() []string {
	if CurrentCertSANs == nil {
		return nil
	}
	return CurrentCertSANs.IdentitySANs()
}

// hostInSANs reports whether hostname — an Origin's url.Hostname(), so never
// bracketed and never carrying a port — is one of the certificate-attested
// names. Both sides go through splitAuthority, so IP SANs compare canonically
// ("::1" == "0:0:0:0:0:0:0:1") and DNS SANs compare case-insensitively.
//
// A leading "*." SAN matches exactly one leftmost label that is non-empty and
// contains no dot (RFC 6125). Without that clause an operator running a
// wildcard certificate would have EVERY origin refused, because the SAN list
// then names no literal host at all.
func hostInSANs(hostname string, sans []string) bool {
	h, _ := splitAuthority(hostname)
	if h == "" {
		return false
	}
	for _, san := range sans {
		s, _ := splitAuthority(san)
		if s == "" {
			continue
		}
		if s == h {
			return true
		}
		if suffix, ok := strings.CutPrefix(s, "*"); ok && strings.HasPrefix(suffix, ".") {
			if label, found := strings.CutSuffix(h, suffix); found && label != "" &&
				!strings.Contains(label, ".") {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 5: Give `isAllowedOrigin` the identity argument**

Replace `isAllowedOrigin`'s signature and body (`internal/web/middleware.go:247-268`) with:

```go
func isAllowedOrigin(origin, networkAccess, effectiveHost, effectiveScheme string, identity []string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	hostname := u.Hostname()
	if hostname == "" {
		return false
	}

	switch networkAccess {
	case "localhost":
		return isLoopback(hostname) || hostname == "localhost" || hostInSANs(hostname, identity)
	case "lan":
		return isLoopback(hostname) || hostname == "localhost" || isPrivateIP(hostname) ||
			hostInSANs(hostname, identity)
	case "external", "public":
		if !sameSiteOrigin(origin, effectiveHost, effectiveScheme) {
			return false
		}
		if len(identity) == 0 {
			return true
		}
		return hostInSANs(hostname, identity)
	default:
		return isLoopback(hostname) || hostname == "localhost" || hostInSANs(hostname, identity)
	}
}
```

Append this to the existing doc comment (which ends at `:246` with the "localhost and lan keep their
IP-class rules exactly as they were." line — replace that line with the text below):

```go
// identity is the certificate-attested host list (identityHosts). On
// external/public it is an ADDITIONAL requirement, never a substitute:
// sameSiteOrigin must still pass, and then the origin's host must also appear
// in the certificate. That conjunction is what closes DNS rebinding — a
// rebinding page controls r.Host and its own Origin, so the pre-existing arm
// compares two values it chose, but "attacker.dns" is in no certificate
// Moombox holds (Arc 5 residual M-8). An install with no certificate, or only
// Moombox's placeholder, has an empty identity and keeps the old behaviour
// exactly; a rebinding attacker who also controls DNS for a name the
// certificate attests is out of scope.
//
// localhost and lan keep their IP-class rules and gain identity as a pure
// WIDENING: both arms reject every DNS name, and the WebSocket upgrade now
// routes through this function, so without it an install holding a real
// certificate for "dash.lan" would lose the socket it has today.
```

- [ ] **Step 6: Extend the `isAllowedOrigin` table**

In `internal/web/middleware_test.go`, add one field to the table struct (`:71-81`), after `scheme`:

```go
		// identity is the certificate-attested host list. nil means "no
		// certificate", which is what every pre-existing row wants.
		identity []string
```

Append these ten rows to the table, after the existing "external mode rejects a host that merely
shares a suffix" row (`:216`):

```go
		// The certificate-identity rows (chain-close O3a), and the mutants each
		// one kills: making the identity arm REPLACE sameSiteOrigin instead of
		// conjoining it fails the ":8080" row; dropping hostInSANs from the
		// external arm fails the rebinding row; denying when identity is empty
		// fails the certless row; dropping the "*." clause fails the wildcard
		// row; letting the wildcard span a dot fails the two-label row;
		// dropping hostInSANs from the lan arm fails the "dash.lan" row.
		{
			name:          "external mode allows a certificate-attested host",
			origin:        "http://dash.example",
			networkAccess: "external",
			host:          "dash.example",
			identity:      []string{"dash.example"},
			expected:      true,
		},
		{
			name:          "external mode refuses a rebinding page once a certificate names the deployment",
			origin:        "http://attacker.dns",
			networkAccess: "external",
			host:          "attacker.dns", // the rebinding page controls BOTH
			identity:      []string{"dash.example"},
			expected:      false,
		},
		{
			name:          "external mode without a certificate keeps the same-host rule alone",
			origin:        "http://attacker.dns",
			networkAccess: "external",
			host:          "attacker.dns",
			identity:      nil,
			expected:      true,
		},
		{
			name:          "external mode still compares ports with a certificate present",
			origin:        "http://dash.example:8080",
			networkAccess: "external",
			host:          "dash.example:774",
			identity:      []string{"dash.example"},
			expected:      false,
		},
		{
			name:          "external mode still allows a TLS-terminated portless pair",
			origin:        "https://dash.example",
			networkAccess: "external",
			host:          "dash.example",
			identity:      []string{"dash.example"},
			expected:      true,
		},
		{
			name:          "external mode expands a wildcard SAN by one label",
			origin:        "https://dash.example.com",
			networkAccess: "external",
			host:          "dash.example.com",
			identity:      []string{"*.example.com"},
			expected:      true,
		},
		{
			name:          "external mode refuses a wildcard SAN spanning two labels",
			origin:        "https://a.b.example.com",
			networkAccess: "external",
			host:          "a.b.example.com",
			identity:      []string{"*.example.com"},
			expected:      false,
		},
		{
			name:          "lan mode allows a certificate-attested name",
			origin:        "https://dash.lan",
			networkAccess: "lan",
			identity:      []string{"dash.lan"},
			expected:      true,
		},
		{
			name:          "lan mode still refuses a bare name with no certificate",
			origin:        "https://dash.lan",
			networkAccess: "lan",
			identity:      nil,
			expected:      false,
		},
		{
			name:          "localhost mode is unchanged when no certificate is loaded",
			origin:        "http://localhost",
			networkAccess: "localhost",
			identity:      nil,
			expected:      true,
		},
```

Update the runner call (`:233`) and its error message:

```go
			result := isAllowedOrigin(tt.origin, tt.networkAccess, host, scheme, tt.identity)
			if result != tt.expected {
				t.Errorf("isAllowedOrigin(%q, %q, host=%q, scheme=%q, identity=%v) = %v, expected %v",
					tt.origin, tt.networkAccess, host, scheme, tt.identity, result, tt.expected)
			}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run 'TestIdentity|TestIsAllowedOrigin' -v`
Expected: PASS — three `TestIdentity*` tests and every `TestIsAllowedOrigin` subtest, old rows included.

Then run the package and the chain: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/`
Expected: ok for both.

- [ ] **Step 8: Verify one mutant by execution**

Temporarily change `IdentitySANs` to drop the `placeholderCertCN` guard (delete the two-line `if`).
Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run TestIdentitySANsIgnoresTheMoomboxPlaceholder`
Expected: FAIL. Revert the mutation and re-run to confirm PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/web/tls.go internal/web/tls_identity_test.go internal/web/middleware.go internal/web/middleware_test.go
git commit -m "$(cat <<'EOF'
feat(web): certificate-attested hosts narrow the external/public origin rule

isAllowedOrigin gains an identity argument sourced from the loaded TLS
certificate. On external/public it is an additional requirement on top of
sameSiteOrigin, which closes the DNS-rebinding residual (Arc 5 M-8): a
rebinding page controls r.Host and its own Origin, but not a certificate.
IdentitySANs returns nil for Moombox's own placeholder certificate, whose
SANs name the machine rather than the deployment, so certless and
placeholder-only installs keep today's behaviour exactly.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/web/tls.go internal/web/tls_identity_test.go internal/web/middleware.go internal/web/middleware_test.go
```

---

### Task 2: One origin decision, and a log line that names the pair

**Files:**
- Modify: `internal/web/middleware.go:16-60` (`CORSMiddleware`), `:124-181` (`CSRFMiddleware`), plus two new helpers before `identityHosts`
- Modify: `internal/web/server.go:116` (the `CSRFMiddleware` call)
- Test: `internal/web/middleware_test.go` (new `TestCSRFOriginComparison`), `:671` (existing constructor call)
- Modify: `docs/spec/security.md:57-67`

**Interfaces:**
- Consumes: `isAllowedOrigin(origin, networkAccess, effectiveHost, effectiveScheme string, identity []string) bool` and `identityHosts() []string` from Task 1; `effectiveRequestHost(store *config.Store, r *http.Request) string` (`:292`); `effectiveRequestScheme(r *http.Request) string` (`:307`).
- Produces: `func originAllowed(store *config.Store, r *http.Request, origin string) (bool, string)` — returns (allowed, the authority compared against); Task 3 calls it. `func clipForLog(s string) string`. `func CSRFMiddleware(store *config.Store, internalToken string, logger interface{ Warn(msg string, args ...any) }) func(http.Handler) http.Handler` — the THIRD parameter is new.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/middleware_test.go`:

```go
// recordingLogger captures Warn lines so a test can assert the refusal line
// exists and names the pair that was compared.
type recordingLogger struct{ warns []string }

func (l *recordingLogger) Warn(msg string, args ...any) {
	line := msg
	for i := 0; i+1 < len(args); i += 2 {
		line += " " + fmt.Sprint(args[i]) + "=" + fmt.Sprint(args[i+1])
	}
	l.warns = append(l.warns, line)
}

// TestCSRFOriginComparison pins WHICH authority the Origin is compared
// against, through the real middleware.
//
// THE MUTANTS: making originAllowed read r.Host instead of
// effectiveRequestHost fails the trusted-proxy row (403 instead of 200);
// trusting X-Forwarded-Host without the trusted_proxies test fails the
// untrusted row (200 instead of 403); deleting the Warn call fails the log
// assertion.
func TestCSRFOriginComparison(t *testing.T) {
	newStore := func(trusted []string) *config.Store {
		return config.NewStore(&config.MoomboxConfig{
			Network: config.NetworkConfig{
				NetworkAccess:  "public",
				TrustedProxies: trusted,
			},
		}, "")
	}
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(""))
		r.RemoteAddr = "10.1.2.3:44444"
		r.Host = "internal:774"
		r.Header.Set("X-Forwarded-Host", "dash.example")
		r.Header.Set("Origin", "http://dash.example")
		return r
	}

	t.Run("trusted proxy: the forwarded host is the one compared", func(t *testing.T) {
		log := &recordingLogger{}
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore([]string{"10.1.2.3"}), "tok", log)(pass).ServeHTTP(rr, newRequest())
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 — the Origin names the forwarded host", rr.Code)
		}
		if len(log.warns) != 0 {
			t.Fatalf("logged %v on an accepted request, want nothing", log.warns)
		}
	})

	t.Run("untrusted peer: the forwarded host is ignored and the refusal names the pair", func(t *testing.T) {
		log := &recordingLogger{}
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore(nil), "tok", log)(pass).ServeHTTP(rr, newRequest())
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — X-Forwarded-Host from an untrusted peer must not count", rr.Code)
		}
		if len(log.warns) != 1 {
			t.Fatalf("logged %v, want exactly one refusal line", log.warns)
		}
		line := log.warns[0]
		for _, want := range []string{"CSRF: origin refused", "http://dash.example", "internal:774"} {
			if !strings.Contains(line, want) {
				t.Fatalf("refusal line %q does not name %q", line, want)
			}
		}
	})
}
```

Add `"fmt"` to that file's import block if it is not already there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run TestCSRFOriginComparison -v`
Expected: FAIL to compile — `too many arguments in call to CSRFMiddleware`.

- [ ] **Step 3: Add `originAllowed` and `clipForLog`**

In `internal/web/middleware.go`, insert both immediately before `identityHosts`:

```go
// originAllowed is the ONE Origin decision. CORSMiddleware, CSRFMiddleware and
// the WebSocket upgrade all route through it, so the three can never again
// disagree about which authority the request answers as (X-Forwarded-Host from
// a trusted proxy, else r.Host), how ports compare (exactly, once either side
// names one), or which hostnames a certificate attests. Before this, the
// upgrade read r.Host only and wildcarded the port, so a Host-rewriting proxy
// loaded the dashboard and then had every socket refused (Arc 5 arc-close F6).
//
// Returns the authority the origin was compared against as well, so a refusal
// can name the pair without recomputing it.
func originAllowed(store *config.Store, r *http.Request, origin string) (bool, string) {
	var networkAccess string
	store.Read(func(c *config.MoomboxConfig) {
		networkAccess = c.Network.NetworkAccess
	})
	host := effectiveRequestHost(store, r)
	return isAllowedOrigin(origin, networkAccess, host, effectiveRequestScheme(r), identityHosts()), host
}

// clipForLog bounds a header value the CLIENT chose before it reaches the log
// ring buffer the dashboard renders, and drops any invalid UTF-8 the byte cut
// may have left behind.
func clipForLog(s string) string {
	const maxLoggedHeader = 200
	if len(s) > maxLoggedHeader {
		s = s[:maxLoggedHeader] + "…"
	}
	return strings.ToValidUTF8(s, "")
}
```

- [ ] **Step 4: Route CORS and CSRF through it**

In `CORSMiddleware`, replace the `networkAccess` read and the `allowed` computation
(`internal/web/middleware.go:22-30`) with:

```go
			// Decided ONCE: the preflight branch below used to re-run the same
			// comparison, and the two must never be able to disagree.
			allowed := false
			if origin != "" {
				allowed, _ = originAllowed(store, r, origin)
			}
```

In `CSRFMiddleware`, change the signature and drop its own `networkAccess` read
(`:152-155`), then replace the invalid-origin branch (`:172-177`):

```go
func CSRFMiddleware(store *config.Store, internalToken string, logger interface {
	Warn(msg string, args ...any)
}) func(http.Handler) http.Handler {
```

```go
			allowed, comparedHost := originAllowed(store, r, origin)
			if !allowed {
				// One line naming the pair that was compared. The most common
				// cause of a 403 here is a reverse proxy that rewrites Host
				// without being listed in network.trusted_proxies, and without
				// this the operator sees only the browser's console error
				// (Arc 5 Task 1 follow-up). The value is client-chosen, so it
				// is clipped before it reaches the dashboard's log panel;
				// volume is bounded by the per-IP API rate limiter.
				logger.Warn("CSRF: origin refused",
					"origin", clipForLog(origin),
					"host", clipForLog(comparedHost))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"Forbidden: invalid origin"}`))
				return
			}
```

In `internal/web/server.go:116`, pass the server's logger:

```go
	r.Use(CSRFMiddleware(store, token, logger))
```

In `internal/web/middleware_test.go:671`, update the existing constructor call:

```go
			mw := CSRFMiddleware(store, internalToken, &recordingLogger{})
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/`
Expected: ok for both — `TestCSRFOriginComparison`, `TestCSRFMiddleware`,
`TestCORSReflectionFollowsTheOriginPolicy` and `cookies_import_chain_test.go` all green.

- [ ] **Step 6: Update `docs/spec/security.md`**

In the "Origin allowance rules by network_access level" list, append this paragraph to the
`external` / `public` bullet, immediately BEFORE its existing `**Residual:**` paragraph (`:67`):

```markdown
  **Certificate attestation.** When a TLS certificate is loaded and it is not the placeholder
  Moombox generates for itself, the origin's host must ALSO appear among that certificate's SANs —
  `identityHosts` and `hostInSANs` in `internal/web/middleware.go`, sourced from `IdentitySANs` in
  `internal/web/tls.go`. This is an additional requirement on top of the same-host comparison, not a
  substitute for it, and it is what refuses a DNS-rebinding page: such a page controls both `Host`
  and its own `Origin`, so the same-host comparison alone compares two values the attacker chose,
  but `attacker.dns` appears in no certificate Moombox holds. Moombox's own self-signed certificate
  is deliberately excluded — its SANs are `localhost`, `127.0.0.1`, `::1` and whatever interface
  addresses the machine had at first start, which name the machine rather than the address an
  operator points a browser at, so treating them as an allowlist would refuse every external install
  reached by a DNS name or a NATed public address. An install with no certificate, or with only the
  placeholder, therefore behaves exactly as it did before. A `*.` SAN matches one label. The
  `localhost` and `lan` policies gain the SAN list as a widening only: those arms are IP-class tests
  that reject every DNS name, and an install holding a real certificate for its own hostname would
  otherwise lose the WebSocket it has today.
  **Not covered:** a rebinding attacker who also controls DNS for a name the certificate attests.
```

Also append one sentence to the CSRF step-5 bullet (`:100`, "Origin/Referer validation"):

```markdown
A refusal logs exactly one `CSRF: origin refused` line naming the origin and the authority it was compared against, both clipped by `clipForLog` (`internal/web/middleware.go`) before they reach the dashboard's log panel.
```

- [ ] **Step 7: Run the documentation gate**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/`
Expected: ok — every backticked symbol resolves to the declaring file named beside it.

- [ ] **Step 8: Commit**

```bash
git add internal/web/middleware.go internal/web/middleware_test.go internal/web/server.go docs/spec/security.md
git commit -m "$(cat <<'EOF'
refactor(web): one Origin decision behind CORS, CSRF and the upgrade

originAllowed reads network_access, the effective request host and the
certificate identity once and answers for all three callers, so they cannot
drift apart again. A CSRF refusal now logs one line naming the origin and the
authority it was compared against — the missing diagnostic for a reverse proxy
that rewrites Host without being listed in network.trusted_proxies.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/web/middleware.go internal/web/middleware_test.go internal/web/server.go docs/spec/security.md
```

---

### Task 3: The WebSocket upgrade shares that decision

**Files:**
- Modify: `internal/web/websocket.go:59-88` (hub struct), `:132-157` (`HandleUpgrade`), `:465-511` (delete `allowedOriginPatterns`), `:2-14` (imports)
- Modify: `internal/web/server.go:101` (wire the hook)
- Test: `internal/web/websocket_origin_test.go` (create)
- Modify: `docs/spec/security.md:193-200`, `docs/spec/user-interfaces.md:471`

**Interfaces:**
- Consumes: `originAllowed(store, r, origin) (bool, string)` and `clipForLog(string) string` from Task 2; `certWatcherFor` / `useIdentityCert` from Task 1.
- Produces: `WebSocketHub.OriginCheck func(*http.Request) bool` — nil means accept every origin.

- [ ] **Step 1: Write the failing test**

Create `internal/web/websocket_origin_test.go`:

```go
package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// wsOriginFixture starts a server whose only handler is the upgrade, wired to
// the same Origin decision the middleware chain uses.
func wsOriginFixture(t *testing.T, networkAccess string, trusted []string) *httptest.Server {
	t.Helper()
	store := config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{
			NetworkAccess:  networkAccess,
			TrustedProxies: trusted,
		},
	}, "")
	hub := NewWebSocketHub(testWSLogger{})
	hub.OriginCheck = func(r *http.Request) bool {
		ok, _ := originAllowed(store, r, r.Header.Get("Origin"))
		return ok
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() {
		srv.Close()
		hub.Close()
	})
	return srv
}

// upgradeStatus performs one raw WebSocket handshake and returns the status the
// server answered with. Written by hand rather than with websocket.Dial because
// these rows must control the Host header, which net/http takes from req.Host
// and DialOptions does not expose.
func upgradeStatus(t *testing.T, srv *httptest.Server, host, origin string, hdr map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestWebSocketUpgradeSharesTheOriginDecision pins the four ways the upgrade
// used to differ from the CSRF check.
//
// THE MUTANTS: restoring OriginPatterns in place of InsecureSkipVerify lets
// the library's unconditional "Origin == Host" arm (accept.go
// authenticateOrigin) answer the rebinding row with 101; deriving the host
// from r.Host instead of effectiveRequestHost fails the forwarded row;
// restoring the old `hostname+":*"` pattern fails the port row; dropping the
// `origin != ""` guard fails the header-less row and breaks every non-browser
// client.
func TestWebSocketUpgradeSharesTheOriginDecision(t *testing.T) {
	t.Run("a rebinding page is refused even though Origin equals Host", func(t *testing.T) {
		srv := wsOriginFixture(t, "lan", nil)
		if got := upgradeStatus(t, srv, "attacker.dns", "http://attacker.dns", nil); got != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — the library would accept this pair on its own", got)
		}
	})

	t.Run("a trusted proxy's forwarded host is the one compared", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", []string{"127.0.0.1"})
		got := upgradeStatus(t, srv, "internal:774", "http://dash.example",
			map[string]string{"X-Forwarded-Host": "dash.example"})
		if got != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101 — the forwarded host matches the Origin", got)
		}
	})

	t.Run("ports are compared exactly", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", nil)
		if got := upgradeStatus(t, srv, "dash.example:774", "http://dash.example:99", nil); got != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — the ports differ", got)
		}
	})

	t.Run("a request with no Origin header is still accepted", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", nil)
		if got := upgradeStatus(t, srv, "attacker.dns", "", nil); got != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101 — non-browser clients send no Origin", got)
		}
	})
}

// THE MUTANT: leave OriginCheck nil in NewServer — every test above still
// passes (they wire the hook themselves) while the real server accepts
// everything.
func TestNewServerWiresTheWebSocketOriginCheck(t *testing.T) {
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})
	if s.WebSocket().OriginCheck == nil {
		t.Fatal("NewServer left WebSocketHub.OriginCheck nil — the upgrade would accept any origin")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run 'TestWebSocketUpgradeSharesTheOriginDecision|TestNewServerWiresTheWebSocketOriginCheck' -v`
Expected: FAIL to compile — `hub.OriginCheck undefined`.

- [ ] **Step 3: Add the hook and the pre-`Accept` check**

In `internal/web/websocket.go`, add to the `WebSocketHub` struct, after the `ClientIP` field (`:76`):

```go
	// OriginCheck decides whether an upgrade's Origin header is acceptable.
	// Set by NewServer to the SAME decision CORSMiddleware and CSRFMiddleware
	// make (originAllowed, internal/web/middleware.go): X-Forwarded-Host from a
	// trusted proxy, port-exact, certificate-attested on external/public.
	// Nil accepts every origin — only test harnesses leave it nil.
	OriginCheck func(r *http.Request) bool
```

In `HandleUpgrade`, insert this immediately before the `websocket.Accept` call (`:149`), and replace
the `AcceptOptions` literal:

```go
	// Origin policy, decided here rather than by the library. The library's
	// own check cannot express it: authenticateOrigin returns nil
	// unconditionally when Origin == Host, which is exactly the pair a
	// DNS-rebinding page controls, and its OriginPatterns are matched with
	// filepath.Match so a port can only be wildcarded or spelled literally.
	// An EMPTY Origin stays acceptable — non-browser clients send none, and
	// the library allowed them too.
	if origin := r.Header.Get("Origin"); origin != "" && hub.OriginCheck != nil && !hub.OriginCheck(r) {
		hub.logger.Warn("websocket upgrade rejected: origin refused",
			"origin", clipForLog(origin),
			"host", clipForLog(r.Host))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The check above IS the origin policy; skipping the library's leaves
		// exactly one (sweep chain-close F6).
		InsecureSkipVerify: true,
	})
```

Delete `allowedOriginPatterns` entirely (`:465-511`, comment block included). It was the only user of
`net` and `strings` in this file, so remove both from the import block — staticcheck and the compiler
will both object otherwise.

- [ ] **Step 4: Wire it in `NewServer`**

In `internal/web/server.go`, after the existing `ClientIP` assignment (`:101`):

```go
	// ...and the same Origin decision: before this the upgrade read r.Host
	// only and wildcarded the port, so a Host-rewriting reverse proxy loaded
	// the dashboard and then had every socket refused (Arc 5 arc-close F6).
	s.ws.OriginCheck = func(r *http.Request) bool {
		ok, _ := originAllowed(store, r, r.Header.Get("Origin"))
		return ok
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/`
Expected: ok for both, including all existing `TestWebSocketHub*` tests.

- [ ] **Step 6: Verify one mutant by execution**

Temporarily replace `InsecureSkipVerify: true` with `OriginPatterns: []string{}`.
Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run TestWebSocketUpgradeSharesTheOriginDecision/a_rebinding_page -v`
Expected: still PASS (our pre-check fires first) — then ALSO delete the pre-check block and re-run.
Expected: FAIL with `status 101, want 403`. Revert both mutations and re-run to confirm PASS.

- [ ] **Step 7: Update the two spec docs**

In `docs/spec/security.md`, replace the four sentences that begin "The WebSocket upgrade builds its
allowed origins from `r.Host`" and end "is a chain-close residual." (`:196-200`) with:

```markdown
The WebSocket upgrade makes the SAME decision through the same helper: `WebSocketHub.OriginCheck`
(`internal/web/websocket.go`) is wired by `NewServer` to `originAllowed`
(`internal/web/middleware.go`), so a proxy listed in `network.trusted_proxies` satisfies the upgrade
exactly as it satisfies CSRF and CORS, and ports are compared exactly rather than wildcarded. The
check runs before `websocket.Accept`, which is then given `InsecureSkipVerify` — the library's own
check accepts `Origin == Host` unconditionally, which is the pair a DNS-rebinding page controls, and
matches ports with `filepath.Match`. An upgrade carrying no `Origin` header at all is still
accepted, as it was before: browsers always send one, and non-browser clients never do.
```

In `docs/spec/user-interfaces.md`, replace the last sentence of the `**Upgrade:**` paragraph (`:471`,
"Origin validation checks that the request comes from the same origin or a loopback/LAN alias.")
with:

```markdown
Origin validation runs before the handshake and is the same decision `CSRFMiddleware` makes — the same `network_access` policy, the same `X-Forwarded-Host`-from-a-trusted-proxy rule, the same exact port comparison, and the same certificate-SAN requirement on `external`/`public`. An upgrade with no `Origin` header is accepted, which is how non-browser clients connect.
```

- [ ] **Step 8: Run the documentation gate**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/`
Expected: ok. `allowedOriginPatterns` no longer exists, so the citation test fails if any spec
sentence still names it — grep for it: `grep -rn allowedOriginPatterns docs/ internal/` must print
nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/web/websocket.go internal/web/websocket_origin_test.go internal/web/server.go docs/spec/security.md docs/spec/user-interfaces.md
git commit -m "$(cat <<'EOF'
fix(web): the WebSocket upgrade shares the CSRF origin decision

allowedOriginPatterns is gone. The upgrade now calls originAllowed before
websocket.Accept, so it reads X-Forwarded-Host from a trusted proxy, compares
ports exactly, and honours the certificate identity — a Host-rewriting proxy
that loads the dashboard no longer has every socket refused. The library's own
check is skipped because it cannot express the rule: it accepts Origin == Host
unconditionally, which is exactly what a rebinding page controls.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/web/websocket.go internal/web/websocket_origin_test.go internal/web/server.go docs/spec/security.md docs/spec/user-interfaces.md
```

---

### Task 4: The root path serves the cache-busted index

**Files:**
- Modify: `internal/web/server.go:38-70` (Server struct), `:254-299` (`MountStaticFiles`), plus a new `serveIndex` after it
- Test: `internal/web/server_test.go:15-28` (fixture), plus new assertions

**Interfaces:**
- Consumes: `(*Server).trustedCommit() bool` (`:310`), `(*Server).assetETag(fsys fs.FS, name string) string` (`:346`), `(*Server).staticCacheHeaders` (`:329`, unchanged).
- Produces: `Server.indexHTML []byte`; `func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS)`.

- [ ] **Step 1: Write the failing test**

First change the fixture's index so it carries the two tokens the substitution targets. In
`internal/web/server_test.go:22`, replace the `index.html` entry with:

```go
		"index.html": {Data: []byte(`<html><head><link rel="stylesheet" href="/moombox.css" /></head>` +
			`<body><script type="module" src="/app.js"></script></body></html>`)},
```

Then append:

```go
// TestRootServesTheCacheBustedIndex: /, /index.html and every SPA route must
// serve the SUBSTITUTED copy. Before this, only the SPA-fallback branch did —
// the root normalised to "index.html", found it in the FS and served the RAW
// embedded bytes through the FileServer, so the dashboard's own load never got
// a cache-busted app.js (Arc 5 arc-close F8).
//
// THE MUTANT: restore the file-exists branch ahead of the index branch — the
// ?v= assertions see the raw body.
func TestRootServesTheCacheBustedIndex(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	for _, target := range []string{"/", "/index.html", "/jobs/42"} {
		rr := getStatic(t, s, target, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", target, rr.Code)
		}
		body := rr.Body.String()
		for _, want := range []string{`"/app.js?v=abc1234"`, `"/moombox.css?v=abc1234"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: body does not carry %s", target, want)
			}
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", target, got)
		}
		if got := rr.Header().Get("ETag"); got != `"abc1234"` {
			t.Errorf("GET %s: ETag = %q, want the quoted build commit", target, got)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("GET %s: Content-Type = %q, want text/html", target, got)
		}
	}
}

// THE MUTANT: keep the substitution gate at `s.commit != ""` — the body comes
// back carrying "?v=unknown" / "?v=abc-dirty", which names no build, and the
// ETag then describes bytes that are not the ones served.
func TestRootOmitsTheCacheBusterOnAnUntrustedCommit(t *testing.T) {
	for _, commit := range []string{"unknown", "abc1234-dirty"} {
		t.Run(commit, func(t *testing.T) {
			s, fsys := staticFixture(t, commit)
			rr := getStatic(t, s, "/", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rr.Code)
			}
			if strings.Contains(rr.Body.String(), "?v=") {
				t.Errorf("body carries a ?v= for untrusted commit %q: %s", commit, rr.Body.String())
			}
			// With no substitution the served bytes ARE the embedded file, so
			// the content-hash ETag describes them exactly.
			sum := sha256.Sum256(fsys["index.html"].Data)
			want := `"` + hex.EncodeToString(sum[:]) + `"`
			if got := rr.Header().Get("ETag"); got != want {
				t.Errorf("ETag = %q, want the content hash %q", got, want)
			}
			if got := rr.Body.String(); got != string(fsys["index.html"].Data) {
				t.Errorf("body = %q, want the embedded file verbatim", got)
			}
		})
	}
}

// THE MUTANT: write the body with w.Write instead of http.ServeContent — the
// conditional request is answered with a full 200.
func TestRootAnswersConditionalGET(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	first := getStatic(t, s, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first GET")
	}

	second := getStatic(t, s, "/", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried %d body bytes, want 0", second.Body.Len())
	}
}
```

Add `"crypto/sha256"` and `"encoding/hex"` to that file's import block.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run 'TestRoot' -v`
Expected: FAIL — `GET /: body does not carry "/app.js?v=abc1234"` (the raw file is served) and
`Cache-Control = "no-cache"` may pass by accident while `ETag`/`?v=` assertions fail.

- [ ] **Step 3: Move the index onto the Server and gate the substitution**

In `internal/web/server.go`, add a field next to `loginHTML` (`:55`):

```go
	indexHTML   []byte           // Dashboard shell with cache-busted asset URLs; see serveIndex
```

Replace the read-and-substitute block in `MountStaticFiles` (`:257-263`) with:

```go
	// The dashboard shell, read once, with cache-busted asset URLs.
	//
	// The ?v= substitution is gated on trustedCommit, not merely on a non-empty
	// commit: an untrusted commit does not identify the bytes (see
	// trustedCommit), so "?v=unknown" and "?v=<rev>-dirty" name nothing.
	// Omitting them also keeps serveIndex's ETag honest — with no substitution
	// the served bytes ARE the embedded file assetETag hashes.
	s.indexHTML, _ = fs.ReadFile(staticFS, "index.html")
	if s.indexHTML != nil && s.trustedCommit() {
		suffix := "?v=" + s.commit
		s.indexHTML = bytes.ReplaceAll(s.indexHTML, []byte(`"/moombox.css"`), []byte(`"/moombox.css`+suffix+`"`))
		s.indexHTML = bytes.ReplaceAll(s.indexHTML, []byte(`"/app.js"`), []byte(`"/app.js`+suffix+`"`))
	}
```

- [ ] **Step 4: Route the root at the index and add `serveIndex`**

In the `NotFound` handler, insert this immediately after the `urlPath` normalisation (`:279`) and
BEFORE the `staticFS.Open` probe:

```go
		// The root and /index.html serve the SUBSTITUTED copy, not the raw
		// embedded file the FileServer below would find. Those two paths are
		// how the dashboard is actually loaded, so they are exactly the ones
		// that need the cache-busted asset URLs (Arc 5 arc-close F8).
		if urlPath == "index.html" && s.indexHTML != nil {
			s.serveIndex(w, r, staticFS)
			return
		}
```

Replace the SPA-fallback branch (`:290-295`) with:

```go
		// SPA fallback: serve the same shell for non-file routes.
		if s.indexHTML != nil {
			s.serveIndex(w, r, staticFS)
			return
		}
```

Add `serveIndex` immediately after `MountStaticFiles`:

```go
// serveIndex writes the dashboard shell — the copy MountStaticFiles built —
// for the root, for /index.html, and for every SPA route.
//
// no-cache, never immutable: the shell names the ?v= of the build it belongs
// to, so a cached copy would keep pointing browsers at the PREVIOUS build's
// assets. The ETag makes that revalidation cost 304 bytes; it is the build
// commit on a trusted build, and the embedded file's content hash otherwise —
// correct in both arms because an untrusted build substitutes nothing, so the
// bytes served are the bytes hashed.
//
// Content-Type is set explicitly rather than left to ServeContent's extension
// lookup, which consults the Windows registry and can be overridden there. The
// zero modtime suppresses Last-Modified, which embed.FS could not supply
// anyway; ServeContent answers If-None-Match and HEAD against the ETag.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if tag := s.assetETag(fsys, "index.html"); tag != "" {
		w.Header().Set("ETag", tag)
	}
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexHTML))
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run 'TestRoot|TestStatic|TestCompression' -v`
Expected: PASS — the three new tests plus `TestStaticAssetsAnswerConditionalGET`,
`TestStaticAssetETagFallsBackToContentHash`, `TestStaticCachePolicyFollowsTheCacheBuster`
(the asset immutable policy is untouched) and `TestStaticCachePolicyNeedsATrustedCommit`.

Then: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/`
Expected: ok for both.

- [ ] **Step 6: Verify one mutant by execution**

Temporarily change the substitution gate back to `s.commit != ""`.
Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ -run TestRootOmitsTheCacheBusterOnAnUntrustedCommit -v`
Expected: FAIL with `body carries a ?v= for untrusted commit "unknown"`. Revert and re-run.

- [ ] **Step 7: Commit**

```bash
git add internal/web/server.go internal/web/server_test.go
git commit -m "$(cat <<'EOF'
fix(web): / and /index.html serve the cache-busted shell

The root normalised to index.html, found it in the embedded FS and served the
raw bytes, so the substituted copy only ever reached the SPA fallback — the
dashboard's own load never got a cache-busted app.js. One serveIndex helper now
answers all three paths with no-cache + ETag through http.ServeContent, and the
?v= substitution is gated on trustedCommit so an untrusted build emits no
cache-buster at all instead of "?v=unknown".

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/web/server.go internal/web/server_test.go
```

---

### Task 5: Full gate run and plan removal

**Files:**
- Delete: `docs/superpowers/plans/2026-09-15-followup-b-web.md`

**Interfaces:**
- Consumes: everything Tasks 1-4 produced.
- Produces: nothing — this task's deliverable is a clean gate log plus the removal.

- [ ] **Step 1: Run the full branch gate list**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/ ./internal/web/routes/ \
  ./internal/config/ ./internal/docs/
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```

Expected: four `ok` lines; `gofmt -l` prints nothing; `go vet`, `staticcheck` and all three builds
silent. `staticcheck` is the gate that catches a stranded `allowedOriginPatterns` or an unused
`net`/`strings` import from Task 3 — if it reports U1000 or a compile error, fix it here rather than
deferring.

- [ ] **Step 2: Confirm the deleted symbol left no references**

```bash
grep -rn "allowedOriginPatterns" . --include=*.go --include=*.md
```
Expected: no output.

- [ ] **Step 3: Delete the plan**

```bash
git rm docs/superpowers/plans/2026-09-15-followup-b-web.md
```

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
chore: remove implemented followup-b-web plan

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- docs/superpowers/plans/2026-09-15-followup-b-web.md
```

---

## Self-Review

**Spec coverage.** O3a → Tasks 1 and 2 (certificate identity, the conjunction on external/public,
the widening on localhost/lan, the refusal log line the spec asked for "if cheap"). O3b → Task 3
(`effectiveRequestHost` via the shared `originAllowed`, port-exact, `security.md` + `user-interfaces.md`
updated in the same task). O5 → Task 4 (root and `/index.html` on the substituted copy, no-cache +
ETag, `?v=` omitted on an untrusted commit, asset immutable policy untouched). Spec test rows 1-12
land in Task 1, rows 13-14 in Task 2, rows 15-18 in Task 3, rows 19-23 in Task 4 (row 23 is the
existing `TestStaticCachePolicyFollowsTheCacheBuster`, asserted green rather than rewritten).

**Type consistency.** `isAllowedOrigin` takes five arguments from Task 1 onward and is called with
five in the Task 1 runner and in Task 2's `originAllowed`. `originAllowed` returns
`(bool, string)` in Task 2 and is destructured as `ok, _` in Task 3's two call sites and as
`allowed, comparedHost` in `CSRFMiddleware`. `CSRFMiddleware` takes three arguments from Task 2
onward, updated at both call sites in that same task. `certWatcherFor` / `useIdentityCert` are
declared once in Task 1's new file and are available to Task 3 (same package).

**Protected behaviour.** No client-IP code is touched: `EffectiveClientIP`, `ExtractIP`,
`canonicalizeForwardedIP`, `loadTrustedProxies` and the IPv4-only bind are all read-only here.
`effectiveRequestHost` is read, never edited. The setup wizard's loopback gate, the unbounded cookie
import, and the BotGuard interpreter gate are untouched.

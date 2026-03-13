# Cipher Audit Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the ALR sig transform false-match bug and harden cipher solving against YouTube player changes.

**Architecture:** Surgical in-place hardening of existing `internal/cipher/` package. No restructuring. Widen regex patterns, fix proximity-bounded ALR search, add cache invalidation callback, add diagnostic logging. Test against real player JS (testdata/player_74edf1a3.js).

**Tech Stack:** Go, Goja (JS VM), regexp, standard library

**Spec:** `docs/superpowers/specs/2026-03-12-cipher-audit-design.md`

---

## Chunk 1: Extraction Robustness (Tasks 1–4)

### Task 1: Fix ALR Transform Chain — Proximity-Bounded Iteration

The critical bug: `findAlrTransformChain` finds the FIRST `set("alr","yes")` then matches `(\w+)&&\((\w+)=` across the entire rest of the file, hitting unrelated code 5,413 chars away and extracting garbage `Z.U` as the sig transform. The fix iterates ALL ALR markers and only matches the transform pattern within 200 chars of each marker.

**Files:**
- Modify: `internal/cipher/extractor_full_player.go:83-155` (replace `findAlrTransformChain`)
- Modify: `internal/cipher/extractor_full_player.go:180-183` (call site in `preprocessPlayerFull`)
- Modify: `internal/cipher/extractor_test.go:183-209` (update tests)

**Note:** Task 7 fully replaces `compileSolver` (which has the logging call site). Do NOT modify `solver.go` in this task — Task 7 handles the logging update.

- [ ] **Step 1: Write the failing test for proximity-bounded ALR**

In `internal/cipher/extractor_test.go`, replace `TestFindAlrTransformChain` with a test that has multiple ALR markers, where only the last one (within proximity) is the correct sig function. Also add a test against the real player.

```go
func TestFindAlrTransformChains(t *testing.T) {
	// Multiple ALR markers — only the last one has the transform pattern nearby.
	// The first ALR has a decoy N&&(N= pattern, but it's 250+ chars away (outside
	// the 200-char proximity window). The second ALR has no transform pattern nearby.
	// The third ALR is the correct sig function with the transform within 15 chars.
	js := `g.ba=function(Z,k,N){` +
		`var a=new dv(k);a.get("alr")||a.set("alr","yes");` +
		strings.Repeat("x", 250) + // spacer pushes decoy outside 200-char proximity
		`N&&(N=Z.U,a=k.U);return a};` +
		strings.Repeat("x", 300) + // spacer
		`this.rV.set("alr","yes");w3R(this.A1,N,Z);` + // no transform pattern nearby
		strings.Repeat("x", 300) + // spacer
		`n8=function(Z,k,N){k=k===void 0?"":k;N=N===void 0?"":N;` +
		`Z=new g.sB(Z,!0);Z.set("alr","yes");` +
		`N&&(N=JI(49,1919,f1(33,6560,N)),Z.set(k,UE(65,3973,N)));return Z};`

	chains := findAlrTransformChains(js)
	if len(chains) == 0 {
		t.Fatal("expected at least one ALR transform chain")
	}

	// Should find JI(49,1919,f1(33,6560,sig)) from the last ALR marker
	found := false
	for _, chain := range chains {
		if strings.Contains(chain, "JI(49,1919,f1(33,6560,sig))") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected chain containing JI(49,1919,f1(33,6560,sig)), got %v", chains)
	}

	// Should NOT contain garbage like "Z.U" from the decoy (it's outside proximity)
	for _, chain := range chains {
		if chain == "Z.U" {
			t.Errorf("false match: got garbage chain %q from decoy ALR marker", chain)
		}
	}

	// Exactly one chain should be found (the correct one)
	if len(chains) != 1 {
		t.Errorf("expected exactly 1 chain, got %d: %v", len(chains), chains)
	}
}

func TestFindAlrTransformChains_NotFound(t *testing.T) {
	js := `var foo=function(a){return a+1};`
	chains := findAlrTransformChains(js)
	if len(chains) != 0 {
		t.Errorf("expected no chains, got %v", chains)
	}
}

func TestFindAlrTransformChains_SingleQuote(t *testing.T) {
	// Single-quote variant of the ALR marker
	js := `eg=function(r,p,I){r=new g.pQ(r,!0);r.set('alr','yes');` +
		`I&&(I=fQ(84,4692,fQ(16,4852,I)),r.set(p,RD(10,4164,I)));return r};`
	chains := findAlrTransformChains(js)
	if len(chains) == 0 {
		t.Fatal("expected chain from single-quote ALR marker")
	}
	if !strings.Contains(chains[0], "fQ(84,4692,fQ(16,4852,sig))") {
		t.Errorf("expected fQ(84,4692,fQ(16,4852,sig)), got %q", chains[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run "TestFindAlrTransformChains" ./internal/cipher/`
Expected: FAIL — `findAlrTransformChains` is undefined (function renamed)

- [ ] **Step 3: Implement `findAlrTransformChains`**

In `internal/cipher/extractor_full_player.go`, replace lines 83-155:

```go
// alrMarkers are the string markers used to identify URL builder functions
// that contain the signature decipher chain. Both quote styles are checked.
var alrMarkers = []string{
	`set("alr","yes");`,
	`set('alr','yes');`,
}

// alrTransformHeadPattern matches the start of a transform chain after set("alr","yes").
// Captures the parameter name and the start of the assignment.
var alrTransformHeadPattern = regexp.MustCompile(`(\w+)&&\((\w+)=`)

// alrProximity is the maximum number of characters after an ALR marker to search
// for the transform head pattern. The correct match is typically within 15 chars;
// 200 gives ample room for formatting variations without matching unrelated code.
const alrProximity = 200

// findAlrTransformChains finds signature decipher chains in YouTube's newer
// player.js format. The URL builder function is identified by its set("alr","yes")
// marker. The transform chain after it applies signature decryption.
//
// Returns all valid transform chains found (one per ALR marker with a nearby
// transform pattern). Each chain has the parameter replaced with "sig".
func findAlrTransformChains(playerJS string) []string {
	var chains []string

	for _, marker := range alrMarkers {
		offset := 0
		for {
			idx := strings.Index(playerJS[offset:], marker)
			if idx < 0 {
				break
			}
			absIdx := offset + idx
			afterMarker := absIdx + len(marker)
			offset = afterMarker // advance for next iteration

			// Only search within alrProximity chars of the marker
			searchEnd := afterMarker + alrProximity
			if searchEnd > len(playerJS) {
				searchEnd = len(playerJS)
			}
			nearby := playerJS[afterMarker:searchEnd]

			m := alrTransformHeadPattern.FindStringSubmatchIndex(nearby)
			if m == nil {
				continue
			}

			param := nearby[m[2]:m[3]]
			assignParam := nearby[m[4]:m[5]]
			if param != assignParam {
				continue
			}

			// Extract the transform expression by tracking parenthesis depth.
			// Use the full remaining text (not just nearby) since the expression
			// itself can be longer than alrProximity.
			fullRest := playerJS[afterMarker:]
			exprStart := m[1] // right after "PARAM&&(PARAM="
			depth := 1
			pos := exprStart
			for pos < len(fullRest) {
				ch := fullRest[pos]
				switch ch {
				case '(':
					depth++
				case ')':
					depth--
					if depth == 0 {
						goto done
					}
				case ',':
					if depth == 1 {
						goto done
					}
				case '\'', '"':
					pos = skipStringLiteral(fullRest, pos)
					continue
				}
				pos++
			}
		done:
			if pos <= exprStart {
				continue
			}

			transform := fullRest[exprStart:pos]

			// Replace the parameter name with "sig" using word-boundary matching
			paramRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(param) + `\b`)
			chain := paramRe.ReplaceAllString(transform, "sig")
			chains = append(chains, chain)
		}
	}

	return chains
}
```

- [ ] **Step 4: Update call site in `preprocessPlayerFull`**

In `internal/cipher/extractor_full_player.go`, replace lines 180-183:

```go
	// New: transform chains from set("alr","yes") markers
	for _, sigChain := range findAlrTransformChains(playerJS) {
		sigGenerators = append(sigGenerators, fmt.Sprintf("function(sig){return %s}", sigChain))
	}
```

- [ ] **Step 5: Run all tests**

Run: `go test -v ./internal/cipher/`
Expected: All PASS including new `TestFindAlrTransformChains*` tests

- [ ] **Step 6: Commit**

```bash
git add internal/cipher/extractor_full_player.go internal/cipher/extractor_test.go
git commit -m "fix(cipher): ALR sig transform proximity-bounded iteration

findAlrTransformChain matched unrelated code 5,413 chars from the
ALR marker, extracting garbage 'Z.U' as the sig transform chain.
Now iterates all ALR markers and only matches within 200 chars.
Also checks single-quote variant set('alr','yes')."
```

---

### Task 2: Widen URL Class Detection

The URL class pattern is hardcoded to `g.XXXX` namespace. YouTube could change to `h.XXXX`, `_yt.XXXX`, or bare identifiers.

**Files:**
- Modify: `internal/cipher/extractor_full_player.go:26-41` (URL class patterns and function)
- Modify: `internal/cipher/extractor_test.go` (add tests)

- [ ] **Step 1: Write failing tests for widened patterns**

```go
func TestFindURLClassName(t *testing.T) {
	tests := []struct {
		name     string
		js       string
		expected string
	}{
		{"current g.XX", `(new g.sB(Z,!0)).get("n")`, "g.sB"},
		{"different namespace", `(new h.Foo(Z,!0)).get("n")`, "h.Foo"},
		{"underscore namespace", `(new _yt.xY(Z,!0)).get("n")`, "_yt.xY"},
		{"with true keyword", `(new g.sB(Z,true)).get("n")`, "g.sB"},
		{"bare identifier", `(new UrlBuilder(Z,!0)).get("n")`, "UrlBuilder"},
		{"not found", `var x = 42;`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findURLClassName(tt.js)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run "TestFindURLClassName" ./internal/cipher/`
Expected: FAIL on "different namespace", "with true keyword", "bare identifier" cases

- [ ] **Step 3: Implement widened patterns**

Replace lines 26-41 in `internal/cipher/extractor_full_player.go`:

```go
// urlClassPatterns match the n-param URL class by finding the pattern:
//   new XXXX(url, !0)).get("n")  or  new XXXX(url, true)).get("n")
// The class name changes across player versions (e.g., g.fb → g.sB).
// Patterns are tried in priority order: dotted (!0), dotted (true), bare.
var urlClassPatterns = []*regexp.Regexp{
	// Dotted identifier with !0: new g.sB(Z, !0)).get("n")
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*!0\)\)\.get\("n"\)`),
	// Dotted identifier with true: new g.sB(Z, true)).get("n")
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*true\)\)\.get\("n"\)`),
	// Bare identifier: new UrlBuilder(Z, !0)).get("n") or true
	regexp.MustCompile(`new\s+([a-zA-Z_$][\w$]*)\([^,]+,\s*(?:!0|true)\)\)\.get\("n"\)`),
}

// findURLClassName extracts the URL builder class name (e.g., "g.sB", "h.Foo")
// from the player JS by finding the n-param resolver pattern.
// Tries dotted identifiers first, then bare identifiers.
func findURLClassName(playerJS string) string {
	for _, pattern := range urlClassPatterns {
		m := pattern.FindStringSubmatch(playerJS)
		if m != nil {
			return m[1]
		}
	}
	return ""
}
```

- [ ] **Step 4: Run tests**

Run: `go test -v -run "TestFindURLClassName" ./internal/cipher/`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cipher/extractor_full_player.go internal/cipher/extractor_test.go
git commit -m "fix(cipher): widen URL class detection beyond g. namespace

Support dotted identifiers (g.sB, h.Foo, _yt.xY), true keyword
variant, and bare identifiers. Patterns tried in priority order."
```

---

### Task 3: IIFE Closing Detection + Window Stripping

Harden IIFE closing detection with EOF proximity check. Make `var window=this;` stripping whitespace-tolerant.

**Files:**
- Modify: `internal/cipher/extractor_full_player.go:189-209` (`preprocessPlayerFull`)
- Modify: `internal/cipher/extractor_test.go` (add tests)

- [ ] **Step 1: Write tests**

```go
func TestPreprocessPlayerFull_WindowStripping(t *testing.T) {
	// Whitespace variation in "var window=this;"
	playerJS := `var _yt_player={};(function(g){ var  window = this ;'use strict';var jVV=function(k){return k+"_done"};var eiz;eiz=[jVV];})(_yt_player);`
	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}
	if strings.Contains(code, "var  window = this ;") {
		t.Error("whitespace-variant 'var window=this;' should have been stripped")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run "TestPreprocessPlayerFull_WindowStripping" ./internal/cipher/`
Expected: FAIL

- [ ] **Step 3: Implement IIFE proximity check and window regex**

In `internal/cipher/extractor_full_player.go`, add a package-level regex and update `preprocessPlayerFull`:

```go
// windowThisPattern matches "var window=this;" with flexible whitespace.
var windowThisPattern = regexp.MustCompile(`var\s+window\s*=\s*this\s*;`)
```

Replace lines 189-209 in `preprocessPlayerFull`:

```go
	// Find the IIFE closing point to insert bindings inside the function scope.
	// Main variant: var _yt_player={};(function(g){...})(_yt_player);
	// TV variant: 'use strict';(function(){var window=this;...}).call(this);
	closeIdx := strings.LastIndex(playerJS, "})(")
	if closeIdx >= 0 && len(playerJS)-closeIdx > 200 {
		// Match is too far from EOF — likely inside a string or nested function
		closeIdx = -1
	}
	if closeIdx < 0 {
		// TV/ES6 variant uses }).call(this) instead of })(arg)
		closeIdx = strings.LastIndex(playerJS, "}).call(")
		if closeIdx >= 0 && len(playerJS)-closeIdx > 200 {
			closeIdx = -1
		}
	}
	if closeIdx < 0 {
		return "", fmt.Errorf("could not find IIFE closing bracket")
	}

	// Build solver binding code
	bindingCode := buildSolverBindings(nGenerators, sigGenerators, urlClassName)

	// Insert bindings inside the IIFE, just before the closing })
	modified := playerJS[:closeIdx] + "\n" + bindingCode + "\n" + playerJS[closeIdx:]

	// Strip "var window=this;" (with flexible whitespace) to avoid overriding our setup stubs.
	modified = windowThisPattern.ReplaceAllLiteralString(modified, "")
```

- [ ] **Step 4: Run all cipher tests**

Run: `go test -v ./internal/cipher/`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cipher/extractor_full_player.go internal/cipher/extractor_test.go
git commit -m "fix(cipher): harden IIFE detection and window stripping

IIFE closing bracket must be within 200 chars of EOF to prevent
false matches. Window=this stripping uses regex for whitespace
tolerance."
```

---

### Task 4: Sig Candidate Validation in _multiTry

Add identity-check validation to `_multiTry` so candidates that return input unchanged are rejected (defense-in-depth).

**Files:**
- Modify: `internal/cipher/extractor_full_player.go:300-315` (`fullPlayerSetupCode`, `_multiTry`)

- [ ] **Step 1: Write test for identity rejection**

```go
func TestMultiTryRejectsIdentity(t *testing.T) {
	// _multiTry should reject candidates that return the input unchanged
	code := fullPlayerSetupCode + `
var _result = {};
_result.sig = _multiTry([
  function(x) { return x; },
  function(x) { return x + "_transformed"; }
]);
`
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	if solvers.Sig == nil {
		t.Fatal("expected non-nil sig solver")
	}
	result, err := solvers.Sig("testinput")
	if err != nil {
		t.Fatalf("sig solver: %v", err)
	}
	if result == "testinput" {
		t.Error("_multiTry should reject identity function, but got input back unchanged")
	}
	if result != "testinput_transformed" {
		t.Errorf("expected 'testinput_transformed', got %q", result)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run "TestMultiTryRejectsIdentity" ./internal/cipher/`
Expected: FAIL — identity function is accepted

- [ ] **Step 3: Add identity check to `_multiTry`**

In `internal/cipher/extractor_full_player.go`, update the `_multiTry` function in `fullPlayerSetupCode` (line 307). Change:

```js
                if (typeof _r === "string" && _r.length > 0) return _r;
```

to:

```js
                if (typeof _r === "string" && _r.length > 0 && _r !== _input) return _r;
```

- [ ] **Step 4: Run tests**

Run: `go test -v ./internal/cipher/`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cipher/extractor_full_player.go internal/cipher/extractor_test.go
git commit -m "fix(cipher): reject identity-function sig candidates in _multiTry

_multiTry now checks that candidate output differs from input,
catching garbage transforms that echo the input unchanged."
```

---

## Chunk 2: Cache Invalidation & Logging (Tasks 5–7)

### Task 5: PlayerCache.Remove and Solver.InvalidateSolver

Add targeted cache eviction for when a specific player's solver is known to be broken.

**Files:**
- Modify: `internal/cipher/player_cache.go` (add `Remove` method)
- Modify: `internal/cipher/solver.go` (add `InvalidateSolver` method)
- Modify: `internal/cipher/solver_test.go` (add test)

- [ ] **Step 1: Write test for InvalidateSolver**

In `internal/cipher/solver_test.go`:

```go
func TestInvalidateSolver(t *testing.T) {
	dir := t.TempDir()
	logger := &testLogger{}
	s, err := NewSolver(dir, logger)
	if err != nil {
		t.Fatalf("NewSolver: %v", err)
	}

	// Manually cache a player file and solver
	playerURL := "https://www.youtube.com/s/player/test123/base.js"
	key := CacheKey(playerURL)

	// Put a fake player JS in the disk cache
	if err := s.playerCache.Put(playerURL, "fake player js"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify disk cache has the file
	cached, err := s.playerCache.Get(playerURL)
	if err != nil || cached == "" {
		t.Fatal("expected cached player JS")
	}

	// Put a fake solver in memory
	s.cacheSolvers(key, &Solvers{})

	// Verify in-memory cache has it
	s.solverMu.RLock()
	_, ok := s.solverData[key]
	s.solverMu.RUnlock()
	if !ok {
		t.Fatal("expected solver in memory cache")
	}

	// Invalidate
	s.InvalidateSolver(playerURL)

	// Verify in-memory cache is cleared
	s.solverMu.RLock()
	_, ok = s.solverData[key]
	s.solverMu.RUnlock()
	if ok {
		t.Error("expected solver evicted from memory cache")
	}

	// Verify disk cache is cleared
	cached, err = s.playerCache.Get(playerURL)
	if err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if cached != "" {
		t.Error("expected disk cache evicted")
	}
}
```

Note: `testLogger` may already exist in `solver_test.go`. If not, add:

```go
type testLogger struct{}
func (testLogger) Debug(string, ...any) {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Error(string, ...any) {}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run "TestInvalidateSolver" ./internal/cipher/`
Expected: FAIL — `InvalidateSolver` undefined

- [ ] **Step 3: Implement `PlayerCache.Remove`**

Add to end of `internal/cipher/player_cache.go`:

```go
// Remove deletes the cached player JS for a specific URL.
// Errors are logged but not returned — removal is best-effort
// (file may be held by another reader on Windows).
func (pc *PlayerCache) Remove(playerURL string) {
	path := pc.FilePath(playerURL)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		if pc.logger != nil {
			pc.logger.Debug("cipher: failed to remove cached player", "path", path, "err", err)
		}
	}
}
```

- [ ] **Step 4: Implement `Solver.InvalidateSolver`**

Add to `internal/cipher/solver.go` after the `InvalidateCache` method (after line 135):

```go
// InvalidateSolver evicts a specific player's solver from both the in-memory
// cache and the disk player cache. Called when a solver is known to be broken
// (e.g., producing 403 errors at runtime).
func (s *Solver) InvalidateSolver(playerURL string) {
	key := CacheKey(playerURL)

	s.solverMu.Lock()
	if _, ok := s.solverData[key]; ok {
		delete(s.solverData, key)
		for i, k := range s.solverOrder {
			if k == key {
				s.solverOrder = append(s.solverOrder[:i], s.solverOrder[i+1:]...)
				break
			}
		}
	}
	s.solverMu.Unlock()

	s.playerCache.Remove(playerURL)
	s.logger.Info("cipher: invalidated solver", "playerID", PlayerIDFromURL(playerURL))
}
```

- [ ] **Step 5: Run tests**

Run: `go test -v ./internal/cipher/`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add internal/cipher/player_cache.go internal/cipher/solver.go internal/cipher/solver_test.go
git commit -m "feat(cipher): add InvalidateSolver for targeted cache eviction

Evicts a specific player's solver from both in-memory LRU and disk
cache. Used when a solver is known to be broken at runtime (403s)."
```

---

### Task 6: Wire Cipher Failure Callback from Engine

Add `OnCipherFailure` callback to `SegmentDownloader` and invoke it from `handleDashError` when 403s (not 410s) occur before any bytes have been written (indicating cipher failure rather than stream end). The check is in `handleDashError` (not `handleGoneError`) because `handleGoneError` doesn't know the status code — it handles both 403 and 410, but only 403 indicates cipher failure.

**Files:**
- Modify: `internal/engine/downloader.go:109-113` (add callback field)
- Modify: `internal/engine/downloader_dash.go:158-181` (invoke callback in `handleDashError`)
- Modify: `internal/worker/strategy_youtube_dash.go:188-205` (wire callback)

- [ ] **Step 1: Add `OnCipherFailure` callback field to `SegmentDownloader`**

In `internal/engine/downloader.go`, add to the callback section (after line 112):

```go
	OnCipherFailure func() // Called once on first 403 before any bytes written (likely cipher issue)
```

Also add a tracking field after `lastHeadProbeTime` (line 106):

```go
	cipherFailureFired bool
```

- [ ] **Step 2: Invoke callback in `handleDashError` (403-specific)**

In `internal/engine/downloader_dash.go`, inside `handleDashError` at line 164 (the `if statusCode == 403 || statusCode == 410` branch), add cipher failure detection BEFORE calling `handleGoneError`. This ensures we only fire on 403, not 410:

```go
	if statusCode == 403 || statusCode == 410 {
		// 403 before any bytes written = likely cipher failure (wrong n-param or sig).
		// 410 = stream ended (not cipher). Only fire on 403.
		if statusCode == 403 && !d.cipherFailureFired && d.bytesWritten.Load() == 0 {
			d.cipherFailureFired = true
			if d.OnCipherFailure != nil {
				d.OnCipherFailure()
			}
		}
		return d.handleGoneError(ctx, consecutiveGoneErrors, hasStartedDownloading)
	}
```

- [ ] **Step 3: Wire from strategy_youtube_dash.go**

In `internal/worker/strategy_youtube_dash.go`, in the `DownloadDash` function, after creating each `SegmentDownloader` (after each `NewSegmentDownloader` call), set the cipher failure callback. Add this block after the video downloader is created (after line 205) and similarly after the audio downloader (after line 228):

For the video downloader (after `result.VideoDownloader = engine.NewSegmentDownloader(...)` around line 205):
```go
		if cipherSolver != nil && videoInfo.PlayerURL != "" {
			result.VideoDownloader.OnCipherFailure = func() {
				job.Logger.Warn("[Cipher] 403 before any data — invalidating solver", "playerURL", videoInfo.PlayerURL)
				cipherSolver.InvalidateSolver(videoInfo.PlayerURL)
			}
		}
```

For the audio downloader (after `result.AudioDownloader = engine.NewSegmentDownloader(...)` around line 228):
```go
		if cipherSolver != nil && videoInfo.PlayerURL != "" {
			result.AudioDownloader.OnCipherFailure = func() {
				job.Logger.Warn("[Cipher] 403 before any data — invalidating solver", "playerURL", videoInfo.PlayerURL)
				cipherSolver.InvalidateSolver(videoInfo.PlayerURL)
			}
		}
```

- [ ] **Step 4: Build and run tests**

Run: `go build ./... && go test ./internal/engine/ ./internal/worker/ ./internal/cipher/`
Expected: Build succeeds, all tests pass

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_dash.go internal/worker/strategy_youtube_dash.go
git commit -m "feat(cipher): wire cache invalidation on 403 before bytes written

OnCipherFailure callback fires once after 3 consecutive 403s when no
bytes have been written, indicating cipher failure. The orchestrator
invalidates the solver so the next attempt refetches the player."
```

---

### Task 7: Extended Diagnostic Logging in compileSolver

Add Debug-level logging for each extraction strategy and Warn when all strategies fail for a component.

**Files:**
- Modify: `internal/cipher/solver.go:102-127` (`compileSolver`)

- [ ] **Step 1: Implement extended logging**

Replace `compileSolver` in `internal/cipher/solver.go`:

```go
func (s *Solver) compileSolver(ctx context.Context, playerURL, playerID string) (*Solvers, error) {
	s.logger.Debug("cipher: fetching player JS", "playerID", playerID)
	playerJS, err := s.playerCache.Fetch(ctx, playerURL)
	if err != nil {
		return nil, fmt.Errorf("fetch player JS for %s: %w", playerID, err)
	}

	// Log extraction strategy results for debugging
	urlClassName := findURLClassName(playerJS)
	nArrayCands := findNArrayCandidates(playerJS)
	sigOldCands := findSigCandidates(playerJS)
	alrSigChains := findAlrTransformChains(playerJS)

	s.logger.Debug("cipher: extraction results", "playerID", playerID, "size", len(playerJS),
		"urlClassName", urlClassName,
		"nArrayCandidates", len(nArrayCands),
		"sigOldCandidates", len(sigOldCands),
		"alrSigChains", len(alrSigChains))

	if urlClassName == "" && len(nArrayCands) == 0 {
		s.logger.Warn("cipher: no n-param strategy succeeded (URL class + array candidates)",
			"playerID", playerID)
	}
	if len(sigOldCands) == 0 && len(alrSigChains) == 0 {
		s.logger.Warn("cipher: no sig strategy succeeded (ALR chains + old candidates)",
			"playerID", playerID)
	}

	preprocessed, err := preprocessPlayer(playerJS)
	if err != nil {
		return nil, fmt.Errorf("preprocess player %s: %w", playerID, err)
	}

	s.logger.Debug("cipher: compiling solver", "playerID", playerID)
	solvers, err := getFromPrepared(preprocessed)
	if err != nil {
		return nil, fmt.Errorf("compile solver for %s: %w", playerID, err)
	}

	return solvers, nil
}
```

- [ ] **Step 2: Run tests**

Run: `go test -v ./internal/cipher/`
Expected: All PASS

- [ ] **Step 3: Commit**

```bash
git add internal/cipher/solver.go
git commit -m "feat(cipher): extended diagnostic logging for extraction strategies

Debug-log each extraction result (URL class, n-array, sig old, ALR
chains). Warn when all strategies for a component fail."
```

---

## Chunk 3: Real Player Validation & End-to-End (Task 8)

### Task 8: Validate Everything Against Real Player

Update the player_74edf1a3 test to actually validate sig solver output (not just nil check), and run the full end-to-end test.

**Files:**
- Modify: `internal/cipher/extractor_test.go:411-463` (`TestPreprocessPlayer74edf1a3`)

- [ ] **Step 1: Update test to validate sig solver output**

Replace `TestPreprocessPlayer74edf1a3` in `internal/cipher/extractor_test.go`:

```go
func TestPreprocessPlayer74edf1a3(t *testing.T) {
	data, err := os.ReadFile("testdata/player_74edf1a3.js")
	if err != nil {
		t.Skip("player_74edf1a3.js not available")
	}

	playerJS := string(data)
	t.Logf("Player size: %d bytes", len(data))

	// Check candidate finding
	nFuncs := findNArrayCandidates(playerJS)
	t.Logf("N-param array candidates: %d %v", len(nFuncs), nFuncs)

	sigCands := findSigCandidates(playerJS)
	t.Logf("Sig old candidates: %d %v", len(sigCands), sigCands)

	alrChains := findAlrTransformChains(playerJS)
	t.Logf("ALR sig chains found: %d", len(alrChains))
	for i, chain := range alrChains {
		t.Logf("  ALR chain %d (len=%d): %s", i, len(chain), chain)
	}

	// The ALR chain must be substantive (not garbage like "Z.U")
	if len(alrChains) > 0 {
		for _, chain := range alrChains {
			if len(chain) < 10 {
				t.Errorf("ALR chain too short (likely false match): %q", chain)
			}
		}
	}

	// URL class detection
	urlClass := findURLClassName(playerJS)
	t.Logf("URL class: %q", urlClass)
	if urlClass == "" {
		t.Error("expected URL class to be detected (e.g., g.sB)")
	}

	// Full preprocessing
	code, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("preprocessPlayerFull: %v", err)
	}
	t.Logf("Preprocessed code: %d bytes", len(code))

	// Execute in Goja
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	// N solver must work and transform the input
	if solvers.N == nil {
		t.Fatal("N solver is nil — this causes 403 on all DASH segments")
	}
	nResult, err := solvers.N("abc123def456")
	if err != nil {
		t.Fatalf("N solver error: %v", err)
	}
	t.Logf("N result: %q (input was abc123def456)", nResult)
	if nResult == "abc123def456" {
		t.Error("N solver returned input unchanged — not actually transforming")
	}
	if nResult == "" {
		t.Error("N solver returned empty string")
	}

	// Sig solver must work and transform the input
	if solvers.Sig == nil {
		t.Fatal("Sig solver is nil — this causes 403 on streams requiring signature decryption")
	}
	sigInput := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	sigResult, err := solvers.Sig(sigInput)
	if err != nil {
		t.Fatalf("Sig solver error: %v", err)
	}
	t.Logf("Sig result: %q (input len=%d, output len=%d)", sigResult, len(sigInput), len(sigResult))
	if sigResult == sigInput {
		t.Error("Sig solver returned input unchanged — not actually transforming")
	}
	if sigResult == "" {
		t.Error("Sig solver returned empty string")
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test -v -run "TestPreprocessPlayer74edf1a3" ./internal/cipher/`
Expected: PASS — N solver transforms input, Sig solver transforms input (no longer garbage)

- [ ] **Step 3: Run the full test suite**

Run: `go test -v ./internal/cipher/`
Expected: All PASS

- [ ] **Step 4: Run full project build and all tests**

Run: `go build ./... && go test ./...`
Expected: Build succeeds, all tests pass

- [ ] **Step 5: Commit**

```bash
git add internal/cipher/extractor_test.go
git commit -m "test(cipher): validate sig solver actually transforms input

TestPreprocessPlayer74edf1a3 now calls Sig solver and verifies
output differs from input. Previously only checked non-nil, missing
the false ALR match bug."
```

---

## Summary of All Changes

| Task | File | What |
|------|------|------|
| 1 | `extractor_full_player.go` | `findAlrTransformChains` — proximity-bounded, multi-marker, single-quote |
| 2 | `extractor_full_player.go` | `urlClassPatterns` — widened from `g.` to any dotted/bare identifier |
| 3 | `extractor_full_player.go` | IIFE EOF proximity check + `windowThisPattern` regex |
| 4 | `extractor_full_player.go` | `_multiTry` identity-check (`_r !== _input`) |
| 5 | `player_cache.go` | `Remove(playerURL)` method |
| 5 | `solver.go` | `InvalidateSolver(playerURL)` method |
| 6 | `downloader.go` | `OnCipherFailure` callback + `cipherFailureFired` |
| 6 | `downloader_dash.go` | Fire callback on 403 before bytes written |
| 6 | `strategy_youtube_dash.go` | Wire callback to `InvalidateSolver` |
| 7 | `solver.go` | Extended Debug/Warn logging in `compileSolver` |
| 8 | `extractor_test.go` | Full sig+n validation against real player |

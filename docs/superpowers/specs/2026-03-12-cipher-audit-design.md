# Cipher Solving Audit — Design Spec

**Date:** 2026-03-12
**Scope:** Full-stack surgical hardening of `internal/cipher/` to resist YouTube player changes.

## Context

Commit `5a96980` revealed that hardcoded assumptions in cipher solving (URL class name `g.fb` → `g.sB`) caused DASH n-param extraction to fail, producing infinite 403 retry loops. An audit found additional fragility points and one active bug.

## Critical Bug: ALR Sig Transform False Match

`findAlrTransformChain` uses `strings.Index` to find the FIRST `set("alr","yes");` occurrence, then `FindStringSubmatchIndex` across the entire remaining file to find `(\w+)&&\((\w+)=`. In player `74edf1a3`:

- **4 ALR occurrences** at bytes 697449, 921215, 989089, 1005037
- Only ALR #4 (byte 1005037, function `n8`) is the actual sig function
- The code finds ALR #1, then matches `N&&(N=Z.U` **5,413 chars away** in unrelated code
- Extracts `Z.U` (len=3) as the transform chain — garbage
- The existing test only checks `solvers.Sig != nil`, never validates the output
- Result: sig solver exists but always throws at runtime

**Correct transform at ALR #4:** `JI(49,1919,f1(33,6560,sig))` — found within 14 chars of the ALR marker.

## Changes

### 1. ALR Transform Chain — Proximity-Bounded Iteration (Bug Fix)

**File:** `extractor_full_player.go`

Change `findAlrTransformChain` to:
1. Find ALL `set("alr","yes")` occurrences (also check single-quote variant `set('alr','yes')`)
2. For each occurrence, search for the `(\w+)&&\((\w+)=` pattern within 200 chars
3. Extract the transform chain from each valid match
4. Return ALL valid chains as a slice (not just one string)
5. All chains become sig generator candidates passed to `_multiTry`

**Signature change:** `findAlrTransformChain(playerJS string) string` → `findAlrTransformChains(playerJS string) []string`

### 2. URL Class Detection — Widened Namespace

**File:** `extractor_full_player.go`

Replace single `urlClassPattern` with ordered pattern list:
1. Dotted identifier: `new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*!0\)\)\.get\("n"\)` — matches `g.sB`, `h.Foo`, `_yt.X`
2. Dotted with `true`: `new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*true\)\)\.get\("n"\)`
3. Bare identifier: `new\s+([a-zA-Z_$][\w$]*)\([^,]+,\s*(?:!0|true)\)\)\.get\("n"\)`

`findURLClassName` tries each in order, returns first match.

### 3. IIFE Closing Detection — EOF Proximity Check

**File:** `extractor_full_player.go`

After finding `strings.LastIndex(playerJS, "})(")`, verify the match is within the last 100 chars of the file. If not, try `}).call(`. If neither is near EOF, return error. This prevents matching `})(` inside string literals or function bodies deep in the file.

### 4. `var window=this;` Stripping — Regex

**File:** `extractor_full_player.go`

Replace `strings.Replace(modified, "var window=this;", "", 1)` with regex `var\s+window\s*=\s*this\s*;` to handle whitespace variations.

### 5. Sig Candidate Validation via _multiTry

**File:** `extractor_full_player.go`

Currently n-param candidates are validated at JS level (nBindingTemplate tests with dummy value). Sig candidates are not validated — `_multiTry` wraps them without testing.

Change: Add equivalent validation to the sig binding. The `_multiTry` helper already validates at call time (catches exceptions, checks for non-empty string). The false `Z.U` transform would throw ReferenceError and be skipped by `_multiTry`. However, with the ALR proximity fix, this is defense-in-depth.

Additionally, `_multiTry` should validate that the output differs from input (like n-param validation does), catching identity-function candidates.

### 6. Cache Invalidation on Runtime Failure

**Files:** `solver.go`, `player_cache.go`, wired from `worker/orchestrator_youtube.go`

Add `InvalidateSolver(playerURL string)` to `Solver`:
- Evict from in-memory solver LRU cache
- Evict from disk player cache (delete the cached .js file)

Wire from orchestrator: on first 403 response during segment download when n-param/sig was applied, call `InvalidateSolver` to force re-fetch and recompile on next attempt.

### 7. Diagnostic Logging

**Files:** `extractor_full_player.go`, `solver.go`

Add logger parameter to `preprocessPlayerFull`. Log at Debug level:
- Each extraction strategy attempted and result (found/not found, match details)
- URL class name detected
- Number of ALR chains found, n-array candidates, sig candidates

Log at Warn level when ALL strategies for a component fail:
- `"no n-param strategy succeeded"`
- `"no sig strategy succeeded"`

### 8. Test Improvements

**File:** `extractor_test.go`

- `TestPreprocessPlayer74edf1a3`: Add actual sig solver invocation and validate output differs from input
- `TestFindAlrTransformChains`: Test with multiple ALR markers, verify only proximity matches are returned
- `TestFindURLClassName`: Test with widened patterns (dotted, bare)
- `TestSolverInvalidation`: Test that `InvalidateSolver` evicts from both caches
- End-to-end: Verify that player_74edf1a3.js produces working N AND Sig solvers

## Files Modified

| File | Changes |
|------|---------|
| `internal/cipher/extractor_full_player.go` | ALR iteration, URL class patterns, IIFE check, window regex, sig validation, logging |
| `internal/cipher/solver.go` | `InvalidateSolver` method, logger plumbing |
| `internal/cipher/player_cache.go` | `Evict(playerURL)` single-key eviction method |
| `internal/cipher/extractor_test.go` | Enhanced tests for all changes |
| `internal/worker/orchestrator_youtube.go` | Wire cache invalidation on 403 |

## Non-Changes

- **Browser stubs:** Goja lacks ES6 Proxy; existing stubs + panic recovery are sufficient
- **Architecture:** No restructuring — the existing full-player + legacy fallback chain is sound
- **Legacy extractor:** Kept as-is (fallback for pre-modern player formats)
- **STS extraction:** Pattern is stable, no changes needed

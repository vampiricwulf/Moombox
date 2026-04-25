package bgutils

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDescramble(t *testing.T) {
	// Encode test data: "hello" -> each byte - 97 -> base64
	input := []byte("hello")
	scrambled := make([]byte, len(input))
	for i, b := range input {
		scrambled[i] = b - 97
	}
	encoded := base64.StdEncoding.EncodeToString(scrambled)

	result, err := descramble(encoded)
	if err != nil {
		t.Fatalf("descramble: %v", err)
	}
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

func TestSessionData_Expiry(t *testing.T) {
	session := &SessionData{
		PoToken:        "test-token",
		ContentBinding: "test-binding",
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	if time.Now().After(session.ExpiresAt) {
		t.Error("session should not be expired yet")
	}

	expired := &SessionData{
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	if !time.Now().After(expired.ExpiresAt) {
		t.Error("session should be expired")
	}
}

type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any) {}
func (l *testLogger) Info(msg string, args ...any)  {}
func (l *testLogger) Warn(msg string, args ...any)  {}
func (l *testLogger) Error(msg string, args ...any) {}

func TestPotProvider_CacheCleanup(t *testing.T) {
	config := &BgConfig{RequestKey: DefaultRequestKey}
	pp := NewPotProvider(config, &testLogger{})

	// Add an expired session
	pp.sessionCache["expired"] = &SessionData{
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	// Add a valid session
	pp.sessionCache["valid"] = &SessionData{
		PoToken:   "token",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	pp.mu.Lock()
	pp.cleanupExpired()
	pp.mu.Unlock()

	if _, ok := pp.sessionCache["expired"]; ok {
		t.Error("expected expired session to be cleaned up")
	}
	if _, ok := pp.sessionCache["valid"]; !ok {
		t.Error("expected valid session to still exist")
	}
}

func TestPotProvider_InvalidateCaches(t *testing.T) {
	config := &BgConfig{RequestKey: DefaultRequestKey}
	pp := NewPotProvider(config, &testLogger{})

	pp.sessionCache["key1"] = &SessionData{ExpiresAt: time.Now().Add(1 * time.Hour)}
	pp.minterCache["key1"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.InvalidateCaches()

	if len(pp.sessionCache) != 0 {
		t.Error("expected empty session cache after invalidate")
	}
	if len(pp.minterCache) != 0 {
		t.Error("expected empty minter cache after invalidate")
	}
}

func TestBGError(t *testing.T) {
	err := &BGError{Code: ErrVMInit, Message: "test error"}
	expected := "VM_INIT: test error"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

// ---------------------------------------------------------------------------
// descramble edge cases
// ---------------------------------------------------------------------------

func TestDescramble_URLSafeBase64(t *testing.T) {
	// Build a string that encodes cleanly only with URL-safe base64
	input := []byte("test123")
	scrambled := make([]byte, len(input))
	for i, b := range input {
		scrambled[i] = b - 97
	}
	encoded := base64.RawURLEncoding.EncodeToString(scrambled)

	result, err := descramble(encoded)
	if err != nil {
		t.Fatalf("descramble URL-safe: %v", err)
	}
	if result != "test123" {
		t.Errorf("expected 'test123', got %q", result)
	}
}

func TestDescramble_InvalidBase64(t *testing.T) {
	_, err := descramble("!!!not-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDescramble_EmptyInput(t *testing.T) {
	result, err := descramble("")
	if err != nil {
		t.Fatalf("descramble empty: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// randomAlphaNum
// ---------------------------------------------------------------------------

func TestRandomAlphaNum(t *testing.T) {
	tests := []struct {
		name string
		n    int
	}{
		{"length_0", 0},
		{"length_1", 1},
		{"length_13", 13},
		{"length_50", 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := randomAlphaNum(tt.n)
			if len(result) != tt.n {
				t.Errorf("expected length %d, got %d", tt.n, len(result))
			}
			for _, c := range result {
				if !strings.ContainsRune(alphaNum, c) {
					t.Errorf("unexpected character %q in output", c)
				}
			}
		})
	}
}

func TestRandomAlphaNum_NotConstant(t *testing.T) {
	// Two calls with length 20 should almost certainly differ
	a := randomAlphaNum(20)
	b := randomAlphaNum(20)
	if a == b {
		t.Errorf("two random strings are identical: %q", a)
	}
}

// ---------------------------------------------------------------------------
// extractStringFromArrayOrValue
// ---------------------------------------------------------------------------

func TestExtractStringFromArrayOrValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string // JSON
		expected string
	}{
		{"direct_string", `"hello"`, "hello"},
		{"empty_string", `""`, ""},
		{"number", `42`, ""},
		{"null", `null`, ""},
		{"array_with_string", `["first","second"]`, "first"},
		{"array_with_numbers_then_string", `[42, "found"]`, "found"},
		{"empty_array", `[]`, ""},
		{"nested_array", `[[1,2], "nested"]`, "nested"},
		{"array_of_empty_strings", `["",""]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractStringFromArrayOrValue(json.RawMessage(tt.input))
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseChallengeData
// ---------------------------------------------------------------------------

func scrambleString(input string) string {
	data := []byte(input)
	result := make([]byte, len(data))
	for i, b := range data {
		result[i] = b - 97
	}
	return base64.StdEncoding.EncodeToString(result)
}

func TestParseChallengeData_DirectArray(t *testing.T) {
	// When rawArr[1] is not a string (null), the parser falls through to use
	// the raw array directly as the challenge data.
	// Indices: 0=messageID, 1=null (no scramble), 2=url, 3=hash, 4=program, 5=globalName
	raw, _ := json.Marshal([]any{
		"msg-123",                // 0 messageID
		nil,                      // 1 null — skips descrambling
		"https://example.com/bg", // 2 interpreter URL
		"hash-abc",               // 3 interpreter hash
		"program-data",           // 4 program (required)
		"vmGlobalName",           // 5 globalName (required)
	})

	ch, err := parseChallengeData(raw)
	if err != nil {
		t.Fatalf("parseChallengeData: %v", err)
	}
	if ch.MessageID != "msg-123" {
		t.Errorf("MessageID: expected 'msg-123', got %q", ch.MessageID)
	}
	if ch.InterpreterURL != "https://example.com/bg" {
		t.Errorf("InterpreterURL: expected URL, got %q", ch.InterpreterURL)
	}
	if ch.InterpreterHash != "hash-abc" {
		t.Errorf("InterpreterHash: expected 'hash-abc', got %q", ch.InterpreterHash)
	}
	if ch.Program != "program-data" {
		t.Errorf("Program: expected 'program-data', got %q", ch.Program)
	}
	if ch.GlobalName != "vmGlobalName" {
		t.Errorf("GlobalName: expected 'vmGlobalName', got %q", ch.GlobalName)
	}
}

func TestParseChallengeData_FallbackInlineScript(t *testing.T) {
	// Test via descrambled format: inner array has script at index 1 but no URL at index 2.
	// After descrambling, the parser should fall back to inline script.
	innerArr := []any{
		"msg-456",    // 0 messageID
		"inline-js",  // 1 interpreter script (fallback)
		"",           // 2 empty URL
		"hash-def",   // 3
		"my-program", // 4
		"myGlobal",   // 5
	}
	innerJSON, _ := json.Marshal(innerArr)
	scrambled := scrambleString(string(innerJSON))

	outer, _ := json.Marshal([]any{"outer", scrambled})

	ch, err := parseChallengeData(outer)
	if err != nil {
		t.Fatalf("parseChallengeData: %v", err)
	}
	if ch.InterpreterURL != "" {
		t.Errorf("InterpreterURL: expected empty, got %q", ch.InterpreterURL)
	}
	if ch.InterpreterScript != "inline-js" {
		t.Errorf("InterpreterScript: expected 'inline-js', got %q", ch.InterpreterScript)
	}
}

func TestParseChallengeData_WithDescrambling(t *testing.T) {
	// Build the inner challenge data, scramble it, and wrap in outer array
	innerArr := []any{
		"descrambled-msg",  // 0
		"descrambled-js",   // 1
		"https://desc.url", // 2
		"desc-hash",        // 3
		"desc-program",     // 4
		"descGlobal",       // 5
	}
	innerJSON, _ := json.Marshal(innerArr)
	scrambled := scrambleString(string(innerJSON))

	// Outer array: [anything, scrambledString]
	outer, _ := json.Marshal([]any{"outer-msg", scrambled})

	ch, err := parseChallengeData(outer)
	if err != nil {
		t.Fatalf("parseChallengeData with descramble: %v", err)
	}
	if ch.MessageID != "descrambled-msg" {
		t.Errorf("MessageID: expected 'descrambled-msg', got %q", ch.MessageID)
	}
	if ch.Program != "desc-program" {
		t.Errorf("Program: expected 'desc-program', got %q", ch.Program)
	}
	if ch.GlobalName != "descGlobal" {
		t.Errorf("GlobalName: expected 'descGlobal', got %q", ch.GlobalName)
	}
}

func TestParseChallengeData_MissingProgram(t *testing.T) {
	raw, _ := json.Marshal([]any{"msg", "script", "url", "hash"}) // only 4 elements, no program
	_, err := parseChallengeData(raw)
	if err == nil {
		t.Fatal("expected error for missing program/globalName")
	}
}

func TestParseChallengeData_MissingGlobalName(t *testing.T) {
	raw, _ := json.Marshal([]any{"msg", "script", "url", "hash", "program"}) // 5 elements, no globalName
	_, err := parseChallengeData(raw)
	if err == nil {
		t.Fatal("expected error for missing globalName")
	}
}

func TestParseChallengeData_EmptyArray(t *testing.T) {
	_, err := parseChallengeData([]byte("[]"))
	if err == nil {
		t.Fatal("expected error for empty array")
	}
}

func TestParseChallengeData_InvalidJSON(t *testing.T) {
	_, err := parseChallengeData([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// parseIntegrityTokenResponse
// ---------------------------------------------------------------------------

func TestParseIntegrityTokenResponse(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantToken    string
		wantTTL      time.Duration
		wantFallback string
		wantErr      bool
	}{
		{
			name:      "minimal_valid",
			input:     `["abc123", 3600]`,
			wantToken: "abc123",
			wantTTL:   3600 * time.Second,
		},
		{
			name:         "all_fields",
			input:        `["token-xyz", 7200, 5, "fallback-token"]`,
			wantToken:    "token-xyz",
			wantTTL:      7200 * time.Second,
			wantFallback: "fallback-token",
		},
		{
			name:      "invalid_ttl_uses_default",
			input:     `["valid-token", "not-a-number"]`,
			wantToken: "valid-token",
			wantTTL:   3600 * time.Second, // default
		},
		{
			name:    "empty_token",
			input:   `["", 3600]`,
			wantErr: true,
		},
		{
			name:    "too_short",
			input:   `["only-one"]`,
			wantErr: true,
		},
		{
			name:    "empty_array",
			input:   `[]`,
			wantErr: true,
		},
		{
			name:    "invalid_json",
			input:   `not json`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := parseIntegrityTokenResponse([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if data.IntegrityToken != tt.wantToken {
				t.Errorf("IntegrityToken: expected %q, got %q", tt.wantToken, data.IntegrityToken)
			}
			if data.EstimatedTTL != tt.wantTTL {
				t.Errorf("EstimatedTTL: expected %v, got %v", tt.wantTTL, data.EstimatedTTL)
			}
			if data.WebsafeFallbackToken != tt.wantFallback {
				t.Errorf("WebsafeFallbackToken: expected %q, got %q", tt.wantFallback, data.WebsafeFallbackToken)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewPotProvider
// ---------------------------------------------------------------------------

func TestNewPotProvider_DefaultRequestKey(t *testing.T) {
	config := &BgConfig{} // empty RequestKey
	pp := NewPotProvider(config, &testLogger{})
	if config.RequestKey != DefaultRequestKey {
		t.Errorf("expected default request key %q, got %q", DefaultRequestKey, config.RequestKey)
	}
	if pp.sessionCache == nil || pp.minterCache == nil || pp.inflight == nil {
		t.Error("expected initialized maps")
	}
}

func TestNewPotProvider_CustomRequestKey(t *testing.T) {
	config := &BgConfig{RequestKey: "custom-key"}
	_ = NewPotProvider(config, &testLogger{})
	if config.RequestKey != "custom-key" {
		t.Errorf("expected custom key preserved, got %q", config.RequestKey)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.InvalidateIntegrityTokens
// ---------------------------------------------------------------------------

func TestPotProvider_InvalidateIntegrityTokens(t *testing.T) {
	config := &BgConfig{RequestKey: DefaultRequestKey}
	pp := NewPotProvider(config, &testLogger{})

	pp.sessionCache["s1"] = &SessionData{ExpiresAt: time.Now().Add(1 * time.Hour)}
	pp.minterCache["m1"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.InvalidateIntegrityTokens()

	if len(pp.minterCache) != 0 {
		t.Error("expected empty minter cache")
	}
	// Session cache should NOT be cleared
	if len(pp.sessionCache) != 1 {
		t.Error("expected session cache to remain intact")
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GetMinterCacheKeys
// ---------------------------------------------------------------------------

func TestPotProvider_GetMinterCacheKeys(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	keys := pp.GetMinterCacheKeys()
	if len(keys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(keys))
	}

	pp.minterCache["binding-a"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}
	pp.minterCache["binding-b"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	keys = pp.GetMinterCacheKeys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(keys))
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found["binding-a"] || !found["binding-b"] {
		t.Errorf("expected both binding keys, got %v", keys)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.Cleanup
// ---------------------------------------------------------------------------

func TestPotProvider_Cleanup(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})
	pp.sessionCache["s1"] = &SessionData{ExpiresAt: time.Now().Add(1 * time.Hour)}
	pp.minterCache["m1"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.Cleanup()

	if len(pp.sessionCache) != 0 || len(pp.minterCache) != 0 {
		t.Error("expected all caches cleared after Cleanup")
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — session cache hit
// ---------------------------------------------------------------------------

func TestPotProvider_SessionCacheHit(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "test-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "cached-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	session, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.PoToken != "cached-token" {
		t.Errorf("expected cached-token, got %q", session.PoToken)
	}
}

func TestPotProvider_SessionCacheExpired(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "expired-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "old-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(-1 * time.Hour),
	}

	// Also add a minter so it tries minting (which will fail since MintFunc is nil,
	// but we can verify the expired session was removed). Single-minter cache
	// keys on defaultMinterKey now (CRIT-2), so seed that key — the minter
	// is reused for whatever contentBinding the caller asks for.
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			return "fresh-token", nil
		},
	}

	session, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.PoToken != "fresh-token" {
		t.Errorf("expected fresh-token, got %q", session.PoToken)
	}
}

func TestPotProvider_BypassCache(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "bypass-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "cached-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	// With bypassCache=true, should skip BOTH session AND minter caches (match TS).
	// The minter is present but should NOT be used — generateAndMint will fail without
	// network, confirming the minter cache was correctly skipped. Single-minter
	// cache (CRIT-2) keys on defaultMinterKey now.
	minterUsed := false
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			minterUsed = true
			return "bypassed-fresh-token", nil
		},
	}

	_, err := pp.GeneratePoToken(context.Background(), binding, true)
	// bypassCache=true should skip the minter cache and go to generateAndMint.
	// This may succeed (with fallback token from Google) or fail (no network).
	// Either way, the cached minter must NOT have been used.
	if err != nil {
		t.Logf("generateAndMint returned error (expected if no network): %v", err)
	}
	if minterUsed {
		t.Error("bypassCache=true should skip the minter cache, but minter was used")
	}
}

func TestPotProvider_BypassCache_SkipsSession(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "bypass-session"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "cached-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	// Without bypass, should return cached session
	session, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.PoToken != "cached-token" {
		t.Errorf("expected cached-token, got %q", session.PoToken)
	}

	// With bypass, should NOT return cached session — goes to generateAndMint.
	// May succeed (with fallback token from Google) or fail (no network).
	session2, err := pp.GeneratePoToken(context.Background(), binding, true)
	if err != nil {
		t.Logf("generateAndMint returned error (expected if no network): %v", err)
	} else if session2.PoToken == "cached-token" {
		t.Error("bypassCache=true returned the cached session token — should have generated fresh")
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — minter cache hit
// ---------------------------------------------------------------------------

// TestPotProvider_OneMinterServesAllBindings locks the CRIT-2 invariant:
// the cached minter under defaultMinterKey is reused for ANY content
// binding the caller asks for. Pre-CRIT-2 the cache was per-binding,
// so a request for binding-A would miss the binding-B cache and force
// a new BotGuard VM run — wasted work and a Goja VM leak per binding.
func TestPotProvider_OneMinterServesAllBindings(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	mintCalls := 0
	bindingsSeen := []string{}
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			mintCalls++
			bindingsSeen = append(bindingsSeen, cb)
			return "tok-for-" + cb, nil
		},
	}

	// Three different bindings; all must mint via the SAME cached minter.
	for _, b := range []string{"channel-a", "channel-b", "channel-c"} {
		s, err := pp.GeneratePoToken(context.Background(), b, false)
		if err != nil {
			t.Fatalf("binding %q: unexpected error: %v", b, err)
		}
		if s.PoToken != "tok-for-"+b {
			t.Errorf("binding %q: PoToken = %q, want %q", b, s.PoToken, "tok-for-"+b)
		}
	}

	if mintCalls != 3 {
		t.Errorf("MintFunc calls: want 3 (one per binding), got %d", mintCalls)
	}
	if len(bindingsSeen) != 3 || bindingsSeen[0] != "channel-a" || bindingsSeen[1] != "channel-b" || bindingsSeen[2] != "channel-c" {
		t.Errorf("bindingsSeen drift: %v", bindingsSeen)
	}

	// minterCache size MUST stay at 1 — pre-CRIT-2 it would have grown
	// to 3 (one entry per binding).
	if got := len(pp.minterCache); got != 1 {
		t.Errorf("minterCache size = %d, want 1 (single-minter invariant)", got)
	}
	if _, ok := pp.minterCache[defaultMinterKey]; !ok {
		t.Errorf("expected minterCache[defaultMinterKey] to be present")
	}
}

func TestPotProvider_MinterCacheHit(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "minter-binding"
	mintCalls := 0
	// Single-minter cache (CRIT-2): seed defaultMinterKey, not the
	// contentBinding. The minter serves any binding the caller asks for.
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			mintCalls++
			return "minted-token-" + cb[:5], nil
		},
	}

	session, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.PoToken != "minted-token-minte" {
		t.Errorf("expected minted-token-minte, got %q", session.PoToken)
	}
	if mintCalls != 1 {
		t.Errorf("expected 1 mint call, got %d", mintCalls)
	}

	// Verify session was cached
	if cached, ok := pp.sessionCache[binding]; !ok {
		t.Error("expected session to be cached after minting")
	} else if cached.PoToken != session.PoToken {
		t.Errorf("cached token mismatch: %q vs %q", cached.PoToken, session.PoToken)
	}
}

func TestPotProvider_MinterCacheExpired(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "expired-minter"
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(-1 * time.Hour), // expired
		MintFunc: func(cb string) (string, error) {
			t.Fatal("expired minter should not be called")
			return "", nil
		},
	}

	// No valid minter → goes to generateAndMint.
	// May succeed (with fallback token from Google) or fail (no network).
	_, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err != nil {
		t.Logf("generateAndMint returned error (expected if no network): %v", err)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — minter error
// ---------------------------------------------------------------------------

func TestPotProvider_MinterError(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "error-binding"
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			return "", fmt.Errorf("mint failed")
		},
	}

	_, err := pp.GeneratePoToken(context.Background(), binding, false)
	if err == nil {
		t.Fatal("expected error from failed minter")
	}
	if !strings.Contains(err.Error(), "mint") {
		t.Errorf("expected mint error, got: %v", err)
	}

	// Verify no session was cached on error
	if _, ok := pp.sessionCache[binding]; ok {
		t.Error("should not cache session on error")
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — inflight dedup
// ---------------------------------------------------------------------------

func TestPotProvider_InflightDedup(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "dedup-binding"
	mintCalls := 0
	mintCh := make(chan struct{})

	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			mintCalls++
			<-mintCh // block until signaled
			return "dedup-token", nil
		},
	}

	var wg sync.WaitGroup
	results := make([]*SessionData, 3)
	errs := make([]error, 3)

	// Launch 3 concurrent requests for the same binding
	for i := range 3 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = pp.GeneratePoToken(context.Background(), binding, false)
		}(i)
	}

	// Give goroutines time to start and hit inflight dedup
	time.Sleep(50 * time.Millisecond)

	// Unblock the mint
	close(mintCh)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d error: %v", i, err)
		}
	}

	// All should get the same token
	for i, r := range results {
		if r.PoToken != "dedup-token" {
			t.Errorf("goroutine %d: expected 'dedup-token', got %q", i, r.PoToken)
		}
	}

	// MintFunc should have been called exactly once (dedup)
	if mintCalls != 1 {
		t.Errorf("expected 1 mint call (dedup), got %d", mintCalls)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — context cancellation during inflight wait
// ---------------------------------------------------------------------------

func TestPotProvider_ContextCancelDuringInflight(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "cancel-binding"
	blockCh := make(chan struct{})

	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(1 * time.Hour),
		MintFunc: func(cb string) (string, error) {
			<-blockCh // block forever
			return "never-returned", nil
		},
	}

	// First request starts generation
	go func() {
		pp.GeneratePoToken(context.Background(), binding, false)
	}()
	time.Sleep(20 * time.Millisecond)

	// Second request hits inflight wait, but we cancel its context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := pp.GeneratePoToken(ctx, binding, false)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}

	close(blockCh)
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoToken — empty content binding generates default
// ---------------------------------------------------------------------------

func TestPotProvider_EmptyContentBinding(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	// This will try to generate with an auto-generated content binding.
	// May succeed (with fallback token from Google) or fail (no network).
	// Verify it doesn't panic regardless.
	_, err := pp.GeneratePoToken(context.Background(), "", false)
	if err != nil {
		t.Logf("generateAndMint returned error (expected if no network): %v", err)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoTokenString wrapper
// ---------------------------------------------------------------------------

func TestPotProvider_GeneratePoTokenString(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "string-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "my-po-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	token, err := pp.GeneratePoTokenString(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "my-po-token" {
		t.Errorf("expected 'my-po-token', got %q", token)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.GeneratePoTokenSession wrapper
// ---------------------------------------------------------------------------

func TestPotProvider_GeneratePoTokenSession(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "session-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "session-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	token, actualBinding, err := pp.GeneratePoTokenSession(context.Background(), binding, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "session-token" {
		t.Errorf("expected 'session-token', got %q", token)
	}
	if actualBinding != binding {
		t.Errorf("expected binding %q, got %q", binding, actualBinding)
	}
}

// ---------------------------------------------------------------------------
// PotProvider.cleanupExpired — also cleans minter cache
// ---------------------------------------------------------------------------

func TestPotProvider_CleanupExpiredBoth(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	pp.sessionCache["s-expired"] = &SessionData{ExpiresAt: time.Now().Add(-1 * time.Hour)}
	pp.sessionCache["s-valid"] = &SessionData{ExpiresAt: time.Now().Add(1 * time.Hour)}
	pp.minterCache["m-expired"] = &TokenMinter{ExpiresAt: time.Now().Add(-1 * time.Hour)}
	pp.minterCache["m-valid"] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.mu.Lock()
	pp.cleanupExpired()
	pp.mu.Unlock()

	if _, ok := pp.sessionCache["s-expired"]; ok {
		t.Error("expired session not cleaned")
	}
	if _, ok := pp.sessionCache["s-valid"]; !ok {
		t.Error("valid session removed")
	}
	if _, ok := pp.minterCache["m-expired"]; ok {
		t.Error("expired minter not cleaned")
	}
	if _, ok := pp.minterCache["m-valid"]; !ok {
		t.Error("valid minter removed")
	}
}

// ---------------------------------------------------------------------------
// BGError — all error codes
// ---------------------------------------------------------------------------

func TestBGError_AllCodes(t *testing.T) {
	codes := []string{
		ErrBadConfig, ErrRequestFailed, ErrChallengeParse, ErrVMInit,
		ErrVMError, ErrTimeout, ErrIntegrity, ErrAsyncSnapshot, ErrSyncSnapshot,
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			err := &BGError{Code: code, Message: "msg"}
			if !strings.HasPrefix(err.Error(), code+":") {
				t.Errorf("expected prefix %q, got %q", code+":", err.Error())
			}
		})
	}
}

func TestBGError_WithInfo(t *testing.T) {
	err := &BGError{
		Code:    ErrVMError,
		Message: "detail",
		Info:    map[string]any{"key": "val"},
	}
	expected := "VM_ERROR: detail (map[key:val])"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}


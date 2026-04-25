package web

import (
	"strings"
	"testing"
	"time"
)

func TestHashPassword(t *testing.T) {
	as := NewAuthService()

	tests := []struct {
		name     string
		password string
	}{
		{name: "simple password", password: "hello"},
		{name: "empty password", password: ""},
		{name: "long password", password: strings.Repeat("a", 256)},
		{name: "special chars", password: "p@$$w0rd!#%^&*()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := as.HashPassword(tt.password)
			if err != nil {
				t.Fatalf("HashPassword(%q) returned error: %v", tt.password, err)
			}

			// Verify format: "scrypt:<salt_hex>:<hash_hex>"
			parts := strings.SplitN(hash, ":", 3)
			if len(parts) != 3 {
				t.Errorf("expected 3 colon-separated parts, got %d in %q", len(parts), hash)
			}
			if parts[0] != "scrypt" {
				t.Errorf("expected prefix 'scrypt', got %q", parts[0])
			}
			// Salt should be 16 bytes = 32 hex chars
			if len(parts[1]) != 32 {
				t.Errorf("expected salt length 32 hex chars, got %d", len(parts[1]))
			}
			// Hash should be 64 bytes = 128 hex chars
			if len(parts[2]) != 128 {
				t.Errorf("expected hash length 128 hex chars, got %d", len(parts[2]))
			}
		})
	}
}

func TestHashPasswordRandomSalt(t *testing.T) {
	as := NewAuthService()

	hash1, err := as.HashPassword("same_password")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	hash2, err := as.HashPassword("same_password")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}

	if hash1 == hash2 {
		t.Error("expected different hashes for same password (random salt), got identical hashes")
	}
}

func TestVerifyPassword(t *testing.T) {
	as := NewAuthService()

	tests := []struct {
		name       string
		password   string
		setupHash  func() string // returns stored hash
		expected   bool
	}{
		{
			name:     "correct password",
			password: "mypassword",
			setupHash: func() string {
				h, _ := as.HashPassword("mypassword")
				return h
			},
			expected: true,
		},
		{
			name:     "wrong password",
			password: "wrongpassword",
			setupHash: func() string {
				h, _ := as.HashPassword("mypassword")
				return h
			},
			expected: false,
		},
		{
			name:     "empty password matches empty hash",
			password: "",
			setupHash: func() string {
				h, _ := as.HashPassword("")
				return h
			},
			expected: true,
		},
		{
			name:     "empty password against non-empty hash",
			password: "",
			setupHash: func() string {
				h, _ := as.HashPassword("notempty")
				return h
			},
			expected: false,
		},
		{
			name:       "malformed hash - missing prefix",
			password:   "test",
			setupHash:  func() string { return "bcrypt:abc:def" },
			expected:   false,
		},
		{
			name:       "malformed hash - too few parts",
			password:   "test",
			setupHash:  func() string { return "scrypt:abc" },
			expected:   false,
		},
		{
			name:       "malformed hash - empty string",
			password:   "test",
			setupHash:  func() string { return "" },
			expected:   false,
		},
		{
			name:       "malformed hash - invalid salt hex",
			password:   "test",
			setupHash:  func() string { return "scrypt:ZZZZ:abcdef" },
			expected:   false,
		},
		{
			name:       "malformed hash - invalid hash hex",
			password:   "test",
			setupHash:  func() string { return "scrypt:abcdef:ZZZZ" },
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storedHash := tt.setupHash()
			result := as.VerifyPassword(tt.password, storedHash)
			if result != tt.expected {
				t.Errorf("VerifyPassword(%q, %q) = %v, expected %v", tt.password, storedHash, result, tt.expected)
			}
		})
	}
}

func TestCreateSession(t *testing.T) {
	as := NewAuthService()

	token, err := as.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}

	// Token should be 32 bytes = 64 hex chars
	if len(token) != 64 {
		t.Errorf("expected token length 64, got %d", len(token))
	}

	// Token should be valid
	if !as.ValidateSession(token) {
		t.Error("newly created session should be valid")
	}
}

func TestCreateSessionUnique(t *testing.T) {
	as := NewAuthService()

	token1, _ := as.CreateSession()
	token2, _ := as.CreateSession()

	if token1 == token2 {
		t.Error("expected unique session tokens, got identical tokens")
	}
}

func TestValidateSession(t *testing.T) {
	as := NewAuthService()

	tests := []struct {
		name     string
		token    string
		setup    func() string // returns token to validate (if different from tt.token)
		expected bool
	}{
		{
			name:     "valid session",
			setup:    func() string { tok, _ := as.CreateSession(); return tok },
			expected: true,
		},
		{
			name:     "empty token",
			token:    "",
			expected: false,
		},
		{
			name:     "non-existent token",
			token:    "0000000000000000000000000000000000000000000000000000000000000000",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := tt.token
			if tt.setup != nil {
				token = tt.setup()
			}
			result := as.ValidateSession(token)
			if result != tt.expected {
				t.Errorf("ValidateSession(%q) = %v, expected %v", token, result, tt.expected)
			}
		})
	}
}

// TestValidateSessionAndSlideRenewsAfterHalfTTL covers the audit S-3
// sliding-window renewal: a session that's past half the TTL but
// before the absolute expiry should be reported as valid AND
// signalled as "slid" so the caller can re-issue the cookie. Earlier
// validations (within the first half of TTL) skip the slide so the
// hot path stays on the cheap RLock-only check.
func TestValidateSessionAndSlideRenewsAfterHalfTTL(t *testing.T) {
	as := NewAuthService()
	token, err := as.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	// Fresh session: should validate without sliding.
	valid, slid := as.ValidateSessionAndSlide(token)
	if !valid {
		t.Fatal("fresh session should validate")
	}
	if slid {
		t.Error("fresh session should NOT slide (within first half of TTL)")
	}

	// Force the session's createdAt back so it's past the slide threshold
	// but not yet expired. Requires a helper because sessionEntry is
	// package-private.
	as.mu.Lock()
	e := as.sessions[token]
	e.createdAt = time.Now().Add(-(sessionSlideThreshold + time.Hour))
	as.sessions[token] = e
	as.mu.Unlock()

	valid, slid = as.ValidateSessionAndSlide(token)
	if !valid {
		t.Error("aged-but-not-expired session should validate")
	}
	if !slid {
		t.Error("aged session should be slid")
	}

	// After sliding, createdAt should be ~now; subsequent validation
	// should NOT slide again.
	valid, slid = as.ValidateSessionAndSlide(token)
	if !valid || slid {
		t.Errorf("just-slid session: valid=%v slid=%v, want valid=true slid=false", valid, slid)
	}
}

// TestValidateSessionAndSlideExpiredFails verifies the hard-expiry
// boundary: a session past sessionTTL (even with sliding enabled)
// should fail validation. Sliding only renews ACTIVE sessions; an
// abandoned token still expires.
func TestValidateSessionAndSlideExpiredFails(t *testing.T) {
	as := NewAuthService()
	token, err := as.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	// Force createdAt past the absolute TTL.
	as.mu.Lock()
	e := as.sessions[token]
	e.createdAt = time.Now().Add(-(sessionTTL + time.Hour))
	as.sessions[token] = e
	as.mu.Unlock()

	valid, slid := as.ValidateSessionAndSlide(token)
	if valid {
		t.Error("expired session should not validate")
	}
	if slid {
		t.Error("expired session should not slide")
	}
}

func TestInvalidateSession(t *testing.T) {
	as := NewAuthService()

	token, _ := as.CreateSession()
	if !as.ValidateSession(token) {
		t.Fatal("session should be valid before invalidation")
	}

	as.InvalidateSession(token)

	if as.ValidateSession(token) {
		t.Error("session should be invalid after InvalidateSession")
	}
}

func TestInvalidateAllSessions(t *testing.T) {
	as := NewAuthService()

	token1, _ := as.CreateSession()
	token2, _ := as.CreateSession()
	token3, _ := as.CreateSession()

	as.InvalidateAllSessions()

	for _, tok := range []string{token1, token2, token3} {
		if as.ValidateSession(tok) {
			t.Errorf("session %q should be invalid after InvalidateAllSessions", tok)
		}
	}
}

func TestInvalidateOtherSessions(t *testing.T) {
	as := NewAuthService()

	token1, _ := as.CreateSession()
	token2, _ := as.CreateSession()
	token3, _ := as.CreateSession()

	as.InvalidateOtherSessions(token2)

	if as.ValidateSession(token1) {
		t.Error("token1 should be invalidated")
	}
	if !as.ValidateSession(token2) {
		t.Error("token2 (kept) should still be valid")
	}
	if as.ValidateSession(token3) {
		t.Error("token3 should be invalidated")
	}
}

func TestInvalidateOtherSessionsNonExistentKeep(t *testing.T) {
	as := NewAuthService()

	token1, _ := as.CreateSession()

	// Keep a token that doesn't exist — should just clear everything
	as.InvalidateOtherSessions("nonexistent")

	if as.ValidateSession(token1) {
		t.Error("token1 should be invalidated when keepToken doesn't exist")
	}
}

func TestTokenPrefix(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		expected string
	}{
		{"normal token", "abcdef1234567890", "abcdef12"},
		{"short token", "abc", "abc"},
		{"exact 8 chars", "12345678", "12345678"},
		{"empty token", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TokenPrefix(tt.token)
			if result != tt.expected {
				t.Errorf("TokenPrefix(%q) = %q, expected %q", tt.token, result, tt.expected)
			}
		})
	}
}

func TestHashAndVerifyToken(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, expected 64", len(token))
	}

	hash, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	// Valid token should verify
	if !VerifyToken(token, hash) {
		t.Error("VerifyToken should return true for valid token")
	}

	// Wrong token should not verify
	if VerifyToken("wrong_token_value_0000000000000000000000000000000000000000000000000000", hash) {
		t.Error("VerifyToken should return false for wrong token")
	}

	// Malformed hash should not verify
	if VerifyToken(token, "invalid") {
		t.Error("VerifyToken should return false for malformed hash")
	}
}

func TestIsScryptHash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid scrypt hash", "scrypt:abc:def", true},
		{"no prefix", "abc:def:ghi", false},
		{"too few parts", "scrypt:abc", false},
		{"empty string", "", false},
		{"just prefix", "scrypt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsScryptHash(tt.input)
			if result != tt.expected {
				t.Errorf("IsScryptHash(%q) = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsAuthRequired(t *testing.T) {
	tests := []struct {
		name          string
		networkAccess string
		passwordHash  string
		expected      bool
	}{
		{
			name:          "external with password",
			networkAccess: "external",
			passwordHash:  "scrypt:abc:def",
			expected:      true,
		},
		{
			name:          "external without password",
			networkAccess: "external",
			passwordHash:  "",
			expected:      false,
		},
		{
			name:          "local with password",
			networkAccess: "localhost",
			passwordHash:  "scrypt:abc:def",
			expected:      false,
		},
		{
			name:          "local without password",
			networkAccess: "localhost",
			passwordHash:  "",
			expected:      false,
		},
		{
			name:          "lan with password",
			networkAccess: "lan",
			passwordHash:  "scrypt:abc:def",
			expected:      false,
		},
		{
			name:          "public with password",
			networkAccess: "public",
			passwordHash:  "scrypt:abc:def",
			expected:      true,
		},
		{
			name:          "empty network access with password",
			networkAccess: "",
			passwordHash:  "scrypt:abc:def",
			expected:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsAuthRequired(tt.networkAccess, tt.passwordHash)
			if result != tt.expected {
				t.Errorf("IsAuthRequired(%q, %q) = %v, expected %v",
					tt.networkAccess, tt.passwordHash, result, tt.expected)
			}
		})
	}
}

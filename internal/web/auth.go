package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

const (
	sessionTTL     = 24 * time.Hour
	sessionCleanup = 1 * time.Hour
	// sessionSlideThreshold is the elapsed-fraction of sessionTTL past
	// which ValidateSession refreshes the session's createdAt timestamp
	// (sliding-window renewal). Audit reports/web.md S-3. Set to half
	// the TTL so an actively-used session — one validation per visit —
	// gets renewed before the absolute expiry hits, but inactive
	// sessions still expire on schedule.
	sessionSlideThreshold = sessionTTL / 2
	scryptN               = 16384
	scryptR               = 8
	scryptP               = 1
	scryptKeyLen          = 64
	saltLen               = 16
)

// authLogger captures the minimal logging surface used for the session-
// cleanup goroutine's panic-recovery diagnostics. nil-safe.
type authLogger interface {
	Error(msg string, args ...any)
}

// AuthService manages password hashing and session tokens.
type AuthService struct {
	mu       sync.RWMutex
	sessions map[string]sessionEntry
	logger   authLogger // set via SetLogger; may be nil
	done     chan struct{} // closed by Stop() to exit cleanup goroutine
}

// SetLogger wires an optional logger used for the session-cleanup
// goroutine's panic-recovery diagnostics. Without it, panics are swallowed
// silently (the pre-existing behaviour).
func (as *AuthService) SetLogger(l authLogger) {
	as.mu.Lock()
	as.logger = l
	as.mu.Unlock()
}

type sessionEntry struct {
	createdAt time.Time
}

// NewAuthService creates a new auth service and starts session cleanup.
func NewAuthService() *AuthService {
	as := &AuthService{
		sessions: make(map[string]sessionEntry),
	}
	return as
}

// Start begins the session cleanup goroutine.
func (as *AuthService) Start() {
	done := make(chan struct{})
	as.mu.Lock()
	as.done = done
	as.mu.Unlock()

	ticker := time.NewTicker(sessionCleanup)
	go func() {
		defer ticker.Stop()
		defer func() {
			if r := recover(); r != nil {
				// Route through SetLogger's sink if present so a panic in
				// the evict loop doesn't vanish silently.
				as.mu.RLock()
				l := as.logger
				as.mu.RUnlock()
				if l != nil {
					l.Error("auth: session cleanup goroutine panic", "panic", r)
				}
			}
		}()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				as.evictExpired()
			}
		}
	}()
}

// Stop stops the session cleanup goroutine.
func (as *AuthService) Stop() {
	as.mu.Lock()
	done := as.done
	as.done = nil
	as.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// HashPassword hashes a plaintext password using scrypt.
// Returns format: "scrypt:<salt_hex>:<hash_hex>"
func (as *AuthService) HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	hash, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}

	return "scrypt:" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

// VerifyPassword checks a plaintext password against a stored scrypt hash.
func (as *AuthService) VerifyPassword(password, storedHash string) bool {
	// Parse "scrypt:<salt_hex>:<hash_hex>"
	parts := strings.SplitN(storedHash, ":", 3)
	if len(parts) != 3 || parts[0] != "scrypt" {
		return false
	}

	salt, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	if len(salt) != saltLen {
		return false
	}

	expectedHash, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	if len(expectedHash) != scryptKeyLen {
		return false
	}

	hash, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(hash, expectedHash) == 1
}

// CreateSession generates a new session token.
func (as *AuthService) CreateSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	token := hex.EncodeToString(b)

	as.mu.Lock()
	as.sessions[token] = sessionEntry{createdAt: time.Now()}
	as.mu.Unlock()

	return token, nil
}

// ValidateSession checks if a session token is valid and not expired.
// Equivalent to ValidateSessionAndSlide but discards the slide signal
// for callers (e.g. internal cleanup, tests) that don't issue cookies.
func (as *AuthService) ValidateSession(token string) bool {
	valid, _ := as.ValidateSessionAndSlide(token)
	return valid
}

// ValidateSessionAndSlide checks the session token, refreshes its
// createdAt when more than half the TTL has elapsed (sliding-window
// renewal), and returns whether the validation succeeded plus whether
// the session was slid so the caller can re-issue the cookie with a
// fresh Max-Age. Audit reports/web.md S-3.
//
// The "slide only when half the TTL has elapsed" trigger keeps the
// hot path on the cheaper RLock most of the time — fresh validations
// don't incur the write — and bounds the per-session createdAt churn
// to at most a few times per day.
func (as *AuthService) ValidateSessionAndSlide(token string) (valid, slid bool) {
	if token == "" {
		return false, false
	}

	as.mu.RLock()
	entry, ok := as.sessions[token]
	as.mu.RUnlock()

	if !ok {
		return false, false
	}

	elapsed := time.Since(entry.createdAt)
	if elapsed >= sessionTTL {
		return false, false
	}

	if elapsed < sessionSlideThreshold {
		return true, false
	}

	// Past the slide threshold — refresh createdAt so an active user
	// stays logged in indefinitely. Re-check the entry under the
	// writer lock so we don't clobber a concurrent invalidation.
	now := time.Now()
	as.mu.Lock()
	if e, stillOk := as.sessions[token]; stillOk {
		e.createdAt = now
		as.sessions[token] = e
		as.mu.Unlock()
		return true, true
	}
	as.mu.Unlock()
	// Session was invalidated between the RLock and the Lock — race
	// loser. The first check still saw it valid, so honour that for
	// this request; future requests get the invalidation.
	return true, false
}

// InvalidateSession removes a single session.
func (as *AuthService) InvalidateSession(token string) {
	as.mu.Lock()
	delete(as.sessions, token)
	as.mu.Unlock()
}

// InvalidateAllSessions removes all sessions.
func (as *AuthService) InvalidateAllSessions() {
	as.mu.Lock()
	as.sessions = make(map[string]sessionEntry)
	as.mu.Unlock()
}

// InvalidateOtherSessions removes all sessions except the given token.
func (as *AuthService) InvalidateOtherSessions(keepToken string) {
	as.mu.Lock()
	entry, ok := as.sessions[keepToken]
	as.sessions = make(map[string]sessionEntry)
	if ok {
		as.sessions[keepToken] = entry
	}
	as.mu.Unlock()
}

func (as *AuthService) evictExpired() {
	as.mu.Lock()
	defer as.mu.Unlock()

	now := time.Now()
	for token, entry := range as.sessions {
		if now.Sub(entry.createdAt) >= sessionTTL {
			delete(as.sessions, token)
		}
	}
}

// --- Client Token helpers ---

// GenerateToken creates a 32-byte random hex token (64 chars).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenPrefix returns the first 8 hex chars of a raw token, used for indexed DB lookup.
func TokenPrefix(rawToken string) string {
	if len(rawToken) < 8 {
		return rawToken
	}
	return rawToken[:8]
}

// HashToken hashes a raw token using scrypt. Returns "salt_hex:hash_hex" (no "scrypt:" prefix).
func HashToken(token string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(token), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

// VerifyToken checks a raw token against a stored hash ("salt_hex:hash_hex").
func VerifyToken(token, storedHash string) bool {
	saltHex, hashHex, ok := strings.Cut(storedHash, ":")
	if !ok {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) != saltLen {
		return false
	}
	expectedHash, err := hex.DecodeString(hashHex)
	if err != nil || len(expectedHash) != scryptKeyLen {
		return false
	}
	hash, err := scrypt.Key([]byte(token), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(hash, expectedHash) == 1
}

// SetSessionCookie sets the moombox_session cookie on the response.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_session",
		Value:    token,
		Path:     "/",
		MaxAge:   86400, // 24 hours
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// IsScryptHash checks if a string looks like a scrypt hash ("scrypt:<salt>:<hash>").
// Used to detect plaintext passwords in config that need auto-conversion.
func IsScryptHash(s string) bool {
	parts := strings.SplitN(s, ":", 3)
	return len(parts) == 3 && parts[0] == "scrypt"
}

// IsAuthRequired returns true if auth is required based on config.
// Auth is required when network_access is "external" or "public" AND a password hash is set.
func IsAuthRequired(networkAccess, passwordHash string) bool {
	return (networkAccess == "external" || networkAccess == "public") && passwordHash != ""
}

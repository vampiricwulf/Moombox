package bgutils

import (
	"fmt"
	"time"
)

const (
	// BotGuard API endpoints
	GoogleWaaCreateURL    = "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create"
	GoogleWaaGenerateITURL = "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/GenerateIT"
	YouTubeJnnCreateURL   = "https://www.youtube.com/api/jnn/v1/Create"
	YouTubeJnnGenerateITURL = "https://www.youtube.com/api/jnn/v1/GenerateIT"

	// API key for BotGuard requests
	BotGuardAPIKey = "AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw"

	// Default request key for PO token generation
	DefaultRequestKey = "O43z0dpjhgX20SCx4KAo"

	// UserAgentFull is used for BOTH the JS DOM shim navigator.userAgent AND
	// every HTTP request this package makes. Upstream bgutils-js uses a single
	// UA across both surfaces — split UAs give an inconsistent fingerprint
	// between the VM's self-view and the server's view of the client.
	UserAgentFull = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// Timeouts
	BotGuardLoadTimeout = 10 * time.Second
	SnapshotTimeout     = 30 * time.Second
	DefaultMintTimeout  = 3 * time.Second

	// Cache TTLs
	SessionCacheTTL = 6 * time.Hour
)

// BgConfig holds configuration for BotGuard operations.
type BgConfig struct {
	RequestKey    string
	VisitorData   string
	UseYouTubeAPI bool // Use YouTube's JNN endpoint instead of Google's
}

// DescrambledChallenge contains the parsed challenge data from the WAA API.
type DescrambledChallenge struct {
	MessageID         string // Index 0: request message ID
	InterpreterURL    string // URL to fetch interpreter JS (index 2)
	InterpreterScript string // Inline interpreter JS (index 1, fallback)
	InterpreterHash   string
	Program           string
	GlobalName        string
}

// IntegrityTokenData contains the response from the GenerateIT API.
type IntegrityTokenData struct {
	IntegrityToken       string
	EstimatedTTL         time.Duration
	WebsafeFallbackToken string
}

// SessionData contains a cached PO token session.
type SessionData struct {
	PoToken        string
	ContentBinding string
	ExpiresAt      time.Time
}

// TokenMinter holds a minter callback and its expiry.
// The Cleanup function shuts down the underlying BotGuard VM when the minter
// is evicted from cache. The VM must stay alive while the minter is cached
// because MintFunc is a closure over the Goja runtime state.
type TokenMinter struct {
	MintFunc  func(contentBinding string) (string, error)
	ExpiresAt time.Time
	Cleanup   func() // Shuts down BotGuard VM; called when minter is evicted
}

// BGError represents a BotGuard-specific error.
type BGError struct {
	Code    string
	Message string
	Info    map[string]any
}

func (e *BGError) Error() string {
	if e.Info != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Info)
	}
	return e.Code + ": " + e.Message
}

// Error codes
const (
	ErrBadConfig     = "BAD_CONFIG"
	ErrRequestFailed = "REQUEST_FAILED"
	ErrChallengeParse = "CHALLENGE_PARSE"
	ErrVMInit        = "VM_INIT"
	ErrVMError       = "VM_ERROR"
	ErrTimeout       = "TIMEOUT"
	ErrIntegrity     = "INTEGRITY_ERROR"
	ErrAsyncSnapshot = "ASYNC_SNAPSHOT"
	ErrSyncSnapshot  = "SYNC_SNAPSHOT"
)

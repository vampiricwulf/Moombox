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

	// Timeouts. Names reflect what each actually bounds — earlier names
	// (BotGuardLoadTimeout for the callback wait, SnapshotTimeout for the
	// interpreter-load wait) were descriptively inverted. The renamed
	// constants are documented alongside; the old names remain as aliases
	// for any external reference but should not be used in new code.
	//
	// BotGuardCallbackTimeout: wait for the async callback delivered by
	// vm.a() during NewBotGuardClient. If BotGuard never invokes the
	// callback in this window, abort.
	BotGuardCallbackTimeout = 10 * time.Second

	// InterpreterExecutionTimeout: execution budget for vm.RunString on
	// the BotGuard interpreter JS (the big blob). A malformed/malicious
	// script must not burn more than this before we Interrupt.
	InterpreterExecutionTimeout = 30 * time.Second

	// SnapshotDefaultTimeout: default budget for BotGuardClient.Snapshot
	// when the caller passes 0.
	SnapshotDefaultTimeout = 30 * time.Second

	// DefaultMintTimeout: fallback budget when minting takes a value-zero
	// timeout. Used only by the minter wrapper, not by Snapshot anymore.
	DefaultMintTimeout = 3 * time.Second

	// Legacy aliases — kept so historical names continue to resolve while
	// call sites migrate. Prefer the new names above in new code.
	BotGuardLoadTimeout = BotGuardCallbackTimeout
	SnapshotTimeout     = InterpreterExecutionTimeout

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

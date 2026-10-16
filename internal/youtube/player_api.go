package youtube

import (
	"context"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// apiClient is the shared HTTP client for /youtubei calls — every client
// variant the player cascade may try, and the browse requests
// (browse.go). Backed by the shared httpx transport for keep-alive
// amortisation.
var apiClient = httpx.Client(30 * time.Second)

// PotTokenProvider generates PO tokens for Innertube player requests.
// Defined here to avoid an import cycle with the bgutils package;
// *bgutils.PotProvider satisfies this interface.
//
// One method, because one is what the player API uses: both fetch paths mint
// with the video ID as the content binding (yt-dlp's PoTokenContext.PLAYER ->
// (video_id, VIDEO_ID) rule) through the provider's ordinary session cache. A
// challenge-sourced variant used to be declared here; it never had a caller,
// and it was deleted rather than left as an obligation on every implementer
// (owner ruling R1, 2026-09-15). The sidecar protocol that would carry a
// challenge is untouched.
type PotTokenProvider interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
}

// PlayerAPI handles interactions with YouTube's Innertube player API.
type PlayerAPI struct {
	auth *Auth
	// apiKey is read on every player-API request and written by
	// Service.Init's startup homepage fetch. Guarded by apiKeyMu — an
	// unsynchronized string write can tear under the race detector and in
	// theory at runtime (two-word header).
	apiKey   string
	apiKeyMu sync.RWMutex
	// cipherSolver serves GetSts (the signature timestamp a player request
	// carries). Sig and n are not solved here: the worker resolves the
	// chosen format's URL after selection (cipher.ResolveFormatURL).
	cipherSolver *cipher.GojaResolver
	potProvider  PotTokenProvider
	// OnVisitorData is called when visitor data is extracted from a watch page.
	OnVisitorData func(visitorData string)
	// session is the signed-in session's identity as the last watch page
	// stated it — SESSION_INDEX and DELEGATED_SESSION_ID, which
	// GenerateAPIHeaders sends as X-Goog-AuthUser and X-Goog-PageId. A
	// membership held on a brand account exists only under that delegated
	// identity: a credentialed call without the headers is judged as the
	// primary account and refused, so the probe-only calls (no watch page of
	// their own) must carry what the last full extraction learned, as yt-dlp
	// threads the master ytcfg's session into every client call. Captured
	// alongside visitor data (captureVisitorData), sticky: a watch page that
	// names no session (fetched without cookies) does not clear it.
	//
	// sessionAccount is the jar's account (Auth.Identity) the session was
	// read under, and probeYtcfg attaches the session only while the jar
	// still holds that account. A session belongs to the cookies it was read
	// with: once cookies.txt is removed it rode along on requests carrying no
	// credentials at all, and once another account's cookies were imported
	// it named account A's brand page on account B's requests, until a
	// restart or a watch page that happened to state a session — and the
	// feed monitor fetches no watch page. yt-dlp never pairs the two, since
	// it reads the session off the page it fetched with the current cookies.
	// sessionAsked is the account whose homepage read is already claimed
	// (claimSessionRead), so an account that changes at runtime has its
	// session read once, not once per probe. The claim stands for a read in
	// flight or one that came back with a page, whatever the page stated, and
	// only until another account's session takes the slot (captureVisitorData
	// hands it back then). A read that did not come back is handed back
	// (releaseSessionRead): at once when its caller abandoned it, and at
	// sessionRetryAt when it failed. sessionClock is the clock sessionRetryAt
	// is read on (nil: time.Now), a test seam.
	sessionMu          sync.RWMutex
	sessionIndex       *int
	delegatedSessionID string
	sessionAccount     string
	sessionAsked       string
	sessionRetryAt     time.Time
	sessionClock       func() time.Time
	logger             interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewPlayerAPI creates a new PlayerAPI instance.
func NewPlayerAPI(auth *Auth, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *PlayerAPI {
	return &PlayerAPI{
		auth:   auth,
		apiKey: constants.DefaultAPIKey,
		logger: logger,
	}
}

// SetCipherSolver sets the goja cipher resolver GetSts reads (signature
// timestamp lookup).
func (p *PlayerAPI) SetCipherSolver(solver *cipher.GojaResolver) {
	p.cipherSolver = solver
}

// SetPotProvider sets the PO token provider used to inject tokens into WEB-family
// player requests. Pass nil to disable injection (used by tests).
func (p *PlayerAPI) SetPotProvider(pp PotTokenProvider) {
	p.potProvider = pp
}

// clientAcceptsPlayerPoToken returns true for Innertube clients that yt-dlp
// marks as benefiting from a PO token on player requests (WEB-family). The
// PLAYER_PO_TOKEN_POLICY is currently non-required upstream but is expected
// to tighten; supplying a token here is future-proof.
func clientAcceptsPlayerPoToken(c constants.YouTubeClientConfig) bool {
	switch c.ClientName {
	case "WEB", "WEB_SAFARI", "WEB_CREATOR", "WEB_EMBEDDED":
		return true
	default:
		return false
	}
}

// SetAPIKey updates the API key used for YouTube API requests.
func (p *PlayerAPI) SetAPIKey(key string) {
	if key != "" {
		p.apiKeyMu.Lock()
		p.apiKey = key
		p.apiKeyMu.Unlock()
	}
}

// APIKey returns the current Innertube API key (default until Service.Init
// extracts the live one from the homepage).
func (p *PlayerAPI) APIKey() string {
	p.apiKeyMu.RLock()
	defer p.apiKeyMu.RUnlock()
	return p.apiKey
}

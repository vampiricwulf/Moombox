package youtube

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
)

var pathNParamRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// apiClient is a shared HTTP client with a 30s timeout (matching TS fetchWithTimeout).
var apiClient = &http.Client{Timeout: 30 * time.Second}

// PotTokenProvider generates PO tokens for Innertube player requests.
// Defined here to avoid an import cycle with the bgutils package; *bgutils.PotProvider
// satisfies this interface.
type PotTokenProvider interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
}

// PlayerAPI handles interactions with YouTube's Innertube player API.
type PlayerAPI struct {
	auth         *Auth
	apiKey       string
	cipherSolver *cipher.Solver
	potProvider  PotTokenProvider
	// OnVisitorData is called when visitor data is extracted from a watch page.
	OnVisitorData func(visitorData string)
	logger        interface {
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

// SetCipherSolver sets the cipher solver for signature/n-param decryption.
func (p *PlayerAPI) SetCipherSolver(solver *cipher.Solver) {
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
		p.apiKey = key
	}
}

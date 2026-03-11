package youtube

import (
	"net/http"
	"regexp"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
)

var pathNParamRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// apiClient is a shared HTTP client with a 30s timeout (matching TS fetchWithTimeout).
var apiClient = &http.Client{Timeout: 30 * time.Second}

// PlayerAPI handles interactions with YouTube's Innertube player API.
type PlayerAPI struct {
	auth         *Auth
	apiKey       string
	cipherSolver *cipher.Solver
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

// SetAPIKey updates the API key used for YouTube API requests.
func (p *PlayerAPI) SetAPIKey(key string) {
	if key != "" {
		p.apiKey = key
	}
}

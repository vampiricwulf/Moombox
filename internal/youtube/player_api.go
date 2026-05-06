package youtube

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

var pathNParamRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// apiClient is the shared HTTP client for /youtubei player-API calls.
// Backed by the shared httpx transport for keep-alive amortisation
// across the 4-5 client variants the strategy may try (WEB, WEB_SAFARI,
// WEB_CREATOR, WEB_EMBEDDED, ANDROID_VR).
var apiClient = httpx.Client(30 * time.Second)

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
	cipherSolver *cipher.GojaResolver
	// cipher is the routed Solver for sig/n decryption. When non-nil,
	// PlayerAPI uses this for cipher solving (sig flows through the
	// sidecar via the composite policy; n falls back to goja if the
	// sidecar is down). When nil, PlayerAPI degrades to goja-only via
	// cipherSolver.GetSolvers (the legacy path) — same observable
	// behaviour as before this migration.
	//
	// cipherSolver (above) stays around for GetSts (signature timestamp
	// lookup) which isn't part of the Solver interface.
	cipher      cipher.Solver
	potProvider PotTokenProvider
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

// SetCipherSolver sets the goja cipher resolver. Kept for GetSts (signature
// timestamp lookup), which is not part of the cipher.Solver interface.
func (p *PlayerAPI) SetCipherSolver(solver *cipher.GojaResolver) {
	p.cipherSolver = solver
}

// SetCipher wires a routed cipher.Solver for sig/n decryption.
// Optional; when nil, PlayerAPI falls back to the legacy
// GetSolvers/DecryptSig/DecryptN path on cipherSolver.
func (p *PlayerAPI) SetCipher(s cipher.Solver) {
	p.cipher = s
}

// hasCipher returns true when either the routed Solver or the legacy goja
// resolver is available. Used by format-parsing guards.
func (p *PlayerAPI) hasCipher() bool {
	return p.cipher != nil || p.cipherSolver != nil
}

// decryptSig solves sig via the routed cipher.Solver if set, falling
// back to the legacy goja path (GetSolvers + Solvers.DecryptSig) otherwise.
// Returns the decrypted value or an error when no solver can produce a sig
// (e.g. goja extraction failed AND no sidecar is configured).
func (p *PlayerAPI) decryptSig(ctx context.Context, playerURL, encrypted string) (string, error) {
	if p.cipher != nil {
		playerID := cipher.PlayerIDFromURL(playerURL)
		out, err := p.cipher.Sig(ctx, playerID, encrypted)
		if err == nil {
			p.logger.Info("[Cipher] sig decrypted via sidecar", "playerID", playerID)
			return out, nil
		}
		// Fall through to legacy on any error so a transient sidecar
		// failure doesn't take sig down completely. The composite solver
		// already routes around fixable errors internally; reaching here
		// means both sidecar and composite-internal fallback failed.
	}
	out, err := p.decryptSigLegacy(ctx, playerURL, encrypted)
	if err == nil {
		p.logger.Info("[Cipher] sig decrypted via goja fallback")
		return out, nil
	}
	return "", err
}

func (p *PlayerAPI) decryptSigLegacy(ctx context.Context, playerURL, encrypted string) (string, error) {
	if p.cipherSolver == nil {
		return "", cipher.ErrSigUnavailable
	}
	solvers, err := p.cipherSolver.GetSolvers(ctx, playerURL)
	if err != nil {
		return "", err
	}
	if !solvers.HasSig() {
		return "", cipher.ErrSigUnavailable
	}
	return solvers.DecryptSig(encrypted)
}

// decryptN solves the n-param via the routed cipher.Solver if set, falling
// back to the legacy goja path otherwise. No per-call success log — n is
// decrypted once per format URL (often dozens per stream) and per-call
// chatter swamps the rest of the log without diagnostic value. The DASH
// path emits a batch summary in strategy_youtube_dash.go for the bulk
// of n decryptions; format-URL path success is implicit (the URL gets
// used). Failures still log via the caller (decryptNParamStrict).
func (p *PlayerAPI) decryptN(ctx context.Context, playerURL, encrypted string) (string, error) {
	if p.cipher != nil {
		playerID := cipher.PlayerIDFromURL(playerURL)
		out, err := p.cipher.N(ctx, playerID, encrypted)
		if err == nil {
			return out, nil
		}
		// fall through to legacy
	}
	return p.decryptNLegacy(ctx, playerURL, encrypted)
}

func (p *PlayerAPI) decryptNLegacy(ctx context.Context, playerURL, encrypted string) (string, error) {
	if p.cipherSolver == nil {
		return "", fmt.Errorf("cipher: n unavailable for player %s", playerURL)
	}
	solvers, err := p.cipherSolver.GetSolvers(ctx, playerURL)
	if err != nil {
		return "", err
	}
	if !solvers.HasN() {
		return "", fmt.Errorf("cipher: n unavailable for player %s", playerURL)
	}
	return solvers.DecryptN(encrypted)
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

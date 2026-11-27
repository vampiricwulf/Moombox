package cipher

import "errors"

// Exported sentinel errors for cipher-package failures that consumers may
// classify (e.g. cipher-rotation vs network-blip vs extractor-needs-update).
// Producers wrap these with `fmt.Errorf("...: %w", ErrXxx)` so consumers
// can match via `errors.Is(err, ErrXxx)` while still surfacing the
// contextual prefix.
//
// Audit cross-cutting C3 sentinel migration follow-on.
var (
	// ErrExtractorMismatch signals that the goja fallback's extractor, the
	// Go port of ejs (preprocessPlayer), could not prepare the player: it
	// does not parse, it is not in either of the two shapes ejs knows, or no
	// URL-builder function is found in it. Typically means YouTube changed
	// the player in a way ejs does not handle yet.
	ErrExtractorMismatch = errors.New("cipher extractor pattern did not match player JS")

	// ErrPlayerJSFetch signals a network-layer failure fetching the
	// player.js file. Typically transient; consumer can retry with backoff.
	ErrPlayerJSFetch = errors.New("failed to fetch player JS")

	// ErrSigDecrypt signals a JS-execution failure inside DecryptSig. The
	// signature solver ran but produced an error or panicked. Often
	// indicates a cipher rotation that broke the extracted solver.
	ErrSigDecrypt = errors.New("signature decrypt failed")

	// ErrNDecrypt signals a JS-execution failure inside DecryptN. The
	// n-param solver ran but produced an error or panicked. Same family
	// as ErrSigDecrypt; often indicates cipher rotation.
	ErrNDecrypt = errors.New("n-parameter decrypt failed")

	// ErrInputRequired signals a programmer-error: the caller passed an
	// empty player URL or stream URL where one is required.
	ErrInputRequired = errors.New("required cipher input is missing")
)

// ErrSigUnavailable indicates the underlying solver could not extract or
// run a sig algorithm for the requested player. Returned by solvers that
// support the cipher.Solver interface but cannot fulfil sig specifically
// (e.g., the legacy goja extractor on a player whose sig is no longer
// pattern-matchable). Composite solvers may surface this verbatim or
// substitute a higher-quality alternative.
var ErrSigUnavailable = errors.New("cipher: sig unavailable for this player")

// ErrPlayerJSStale signals that the sidecar reported it could not solve a
// challenge against its cached preprocessed player JS — the canonical case
// being ejs's "no solutions" error after YouTube rotates the cipher
// algorithm. The sidecarSolver eats this once: it invalidates disk +
// in-memory + sidecar caches and retries with fresh JS (once per player
// within staleVerdictWindow). A failure that persists is then tried on the
// player's tv body, and only when that fails too does this sentinel reach
// the caller. It then means the sidecar has nothing left for this player:
// ejs cannot solve with its main body or its tv body, a break in ejs or a
// player it does not handle yet. The caller asks goja last
// (compositeSolver.N and Batch for an n, RoutedResolveURL for a signature),
// which does not answer current players; when that fails too the sentinel
// is still the one errors.Is sees, and the worker reads it as a failure
// every format of the player shares (systemicResolveFailure,
// internal/worker/challenge_free.go), not as one format's.
var ErrPlayerJSStale = errors.New("cipher: player JS is stale or unsupported")

// ErrNUnsolved signals a stream URL that carries an n challenge no solver
// produced a valid answer for: every solver failed, or each answered with
// something that is not an answer (checkNResult), or there was no player or
// no solver to ask, or the n does not percent-decode (errNUndecodable), or
// the answer could not be put in the challenge's place.
// RoutedResolveURL, GojaResolver.ResolveURL (and so ResolveFormatURL) and
// RoutedDecryptNInURL return it, wrapping the cause.
//
// It used to be no error at all: the URL went out with its n as YouTube sent
// it, and a Warn said "downloads may be throttled". yt-dlp dropped that
// premise in 800ec085c (2024-06-21, "Skip formats if nsig decoding fails",
// ref youtube-dl#32815), because such URLs are refused outright: an HTTPS
// format whose n has no answer is skipped (`if not n_result: continue`,
// _video.py:3570-3580), and an HLS or DASH manifest is used only `if solved_n
// or not n_challenge` (_video.py:3692-3713, 3730-3751, d20f58d72). The
// consumers here do the same: the VOD and manifest-free strategies treat it
// as their format's resolve failure, and the DASH and HLS strategies fail
// their setup on it rather than download segments that 403.
var ErrNUnsolved = errors.New("cipher: n challenge unsolved")

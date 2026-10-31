package youtube

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
)

// NeedsPlayerJS reports whether f's URL can be fetched only once a player JS
// challenge has been solved: a signatureCipher entry (EncryptedSig, which
// only the sidecar can sign) or an n challenge in the URL
// (cipher.HasNChallenge). Its complement is a copy yt-dlp's
// REQUIRE_JS_PLAYER False clients serve — visionos's, android_vr's — whose
// URL goes out as YouTube sent it.
//
// yt-dlp skips such a format when its challenge cannot be solved, before it
// records the stream (_video.py:3557-3580, 3587-3589), so the next client's
// copy of the stream — one that needs no JS — is the one it keeps. Moombox
// dedups first and resolves after selection, so the worker asks this when a
// resolve fails for a reason every such format shares (its challenge-free
// fallback, audit G10).
func (f *Format) NeedsPlayerJS() bool {
	return f.EncryptedSig != "" || cipher.HasNChallenge(f.URL)
}

// CookielessHlsManifest asks the cookieless clients — visionos, then
// android_vr, the cookieless chain's order (tryCookielessFallbacks) — for a
// live stream's HLS master playlist whose URL carries no n challenge
// (cipher.HasNChallenge), and returns the first one with the client's label
// (the manifest source a VideoInfo stamps). Both clients are yt-dlp's
// REQUIRE_JS_PLAYER False ones (_base.py:285-310), and visionos is the whole
// of its _DEFAULT_JSLESS_CLIENTS (_video.py:144): their live master needs no
// player to be fetched.
//
// It is the worker's challenge-free live fallback (audit G10, owner decision
// 7): when the player's JS challenges cannot be solved, a live capture
// records the HLS playlist, and the master the extraction carries is
// usually tv's or the watch page's, whose path n would need that player.
// yt-dlp uses a manifest only `if solved_n or not n_challenge`
// (_video.py:3692-3713), so a client whose master's n has no answer
// contributes nothing and the next client's master is the one it records.
// A client that fails, refuses the video, serves no master, or one with an n
// is passed over; the error names the last reason when none serves one.
func (p *PlayerAPI) CookielessHlsManifest(ctx context.Context, videoID, visitorData string) (manifestURL, source string, err error) {
	ctx = withExtractionState(ctx)
	lastErr := errors.New("no cookieless client asked")
	for _, fb := range []struct {
		client constants.YouTubeClientConfig
		label  string
	}{
		{constants.VisionOSClient, "visionos"},
		{constants.AndroidVRClient, "android_vr"},
	} {
		info, fetchErr := p.fetchWithCookielessClient(ctx, videoID, visitorData, fb.client)
		switch {
		case fetchErr != nil:
			lastErr = fmt.Errorf("%s: %w", fb.label, fetchErr)
		case info == nil || info.PlayabilityError != PlayabilityOK:
			lastErr = fmt.Errorf("%s: the video is not playable to it", fb.label)
		case info.HlsManifestURL == "":
			lastErr = fmt.Errorf("%s: no HLS manifest", fb.label)
		case cipher.HasNChallenge(info.HlsManifestURL):
			lastErr = fmt.Errorf("%s: its HLS manifest carries an n challenge", fb.label)
		default:
			return info.HlsManifestURL, fb.label, nil
		}
		p.logger.Debug("[PlayerApi] cookieless HLS manifest: client passed over",
			"client", fb.label, "reason", lastErr.Error())
	}
	return "", "", fmt.Errorf("no cookieless client served an HLS manifest that needs no player JS: %w", lastErr)
}

// AuthLevelVisionOSJSLess is visionos's AuthLevel while no player JS
// challenge can be solved (PlayerAPI.SetJSChallengeSolvable): below every
// other level, so its copy of a stream wins the dedup over tv's, the web
// family's and web_embedded's alike — every one of which needs the player JS
// (a signatureCipher signature, an n) — and it leads the selector's last
// tie-break. Its usual level is AuthLevelVisionOS, the last-resort tier;
// see the AuthLevel block in types.go for why, and jslessState for when
// this one applies instead.
const AuthLevelVisionOSJSLess = -1

// SetJSChallengeSolvable installs the report of whether the player's JS
// challenges can be solved right now — cmd/moombox wires the cipher
// composite's SidecarReady: the sidecar is the only signature solver, and
// with it disabled or down only the goja n fallback is left. nil (the
// default, and every test that sets none) answers that they can, which is
// the healthy ranking.
func (p *PlayerAPI) SetJSChallengeSolvable(fn func() bool) {
	p.jsChallengesSolvable = fn
}

// jsLess reports whether an extraction starting now must rank for a
// download without player JS: the hook says the challenges cannot be solved.
func (p *PlayerAPI) jsLess() bool {
	fn := p.jsChallengesSolvable
	return fn != nil && !fn()
}

// jslessState is one extraction's JS-less ranking, owner decision 4 of the
// yt-dlp parity plan: while the cipher sidecar is disabled or down, yt-dlp's
// _DEFAULT_JSLESS_CLIENTS — ('visionos',), the client whose URLs need no player
// JS (_video.py:144, 2972-2977, 2994-3000; dae52d838, #17461) — is what
// upstream asks when no JS runtime is available, with a warning that "some
// formats may be missing". Moombox pools every client rather than picking one,
// so the port is a ranking: visionos's copies outrank every other client's
// (AuthLevelVisionOSJSLess), visionos is asked even where the cascade would not
// have (rankJSLess), and its live HLS master, which carries no n, replaces one
// that does. The healthy ranking is unchanged.
//
// It goes beyond upstream on one side: yt-dlp keeps its authenticated
// tuple for a signed-in session whatever the JS runtime, and every client
// in it needs the player JS, so a signed-in yt-dlp with no runtime has
// nothing to download a public video from; here the signed-in cascade ranks
// visionos first too. A members-only or private video gains nothing — the
// cookieless client is refused it — and loses nothing.
type jslessState struct {
	on    atomic.Bool
	asked atomic.Bool
	hls   atomic.Pointer[visionosHlsMaster]
}

// visionosHlsMaster is the HLS master the extraction's visionos answer
// carried, with its pre-roll instant.
type visionosHlsMaster struct {
	url string
	at  time.Time
}

// markJSLess records, at the head of a cascade, whether it ranks JS-lessly.
// Nil (no cascade) records nothing.
func (s *extractionState) markJSLess(on bool) {
	if s != nil {
		s.jsl.on.Store(on)
	}
}

// jsLess reports whether this extraction ranks JS-lessly. False outside a
// cascade.
func (s *extractionState) jsLess() bool {
	return s != nil && s.jsl.on.Load()
}

// noteVisionOS records that this extraction asked visionos — whatever it
// answered — and the HLS master a playable answer carried, and returns the
// AuthLevel its formats are pooled at: AuthLevelVisionOSJSLess while the
// extraction ranks JS-lessly, AuthLevelVisionOS otherwise.
func (s *extractionState) noteVisionOS(info *VideoInfo, err error) int {
	if s == nil {
		return AuthLevelVisionOS
	}
	s.jsl.asked.Store(true)
	if err == nil && info != nil && info.PlayabilityError == PlayabilityOK && info.HlsManifestURL != "" {
		s.jsl.hls.Store(&visionosHlsMaster{url: info.HlsManifestURL, at: info.HlsManifestAvailableAt})
	}
	if s.jsLess() {
		return AuthLevelVisionOSJSLess
	}
	return AuthLevelVisionOS
}

// rankJSLess is finishExtraction's JS-less step, a no-op while the player's
// JS challenges can be solved. It marks info JSLess, for the worker's
// "formats may be missing" warning; asks visionos, for a playable stream that is
// not still upcoming, where the cascade did not (the public cascade asks
// the cookieless chain only when TV was inadequate, the authenticated one
// only when web_creator was), merging its copies at AuthLevelVisionOSJSLess
// (MergeFormatPool), so they win their streams; and replaces an HLS master
// whose URL carries an n with visionos's, which carries none. A master is
// not added where the extraction found none: that would change which
// strategy a stream is routed to.
func (p *PlayerAPI) rankJSLess(ctx context.Context, info *VideoInfo, wp *WatchPageResult, videoID string) {
	st := extractionStateFrom(ctx)
	if info == nil || !st.jsLess() {
		return
	}
	info.JSLess = true
	if !st.jsl.asked.Load() && info.PlayabilityError == PlayabilityOK && info.StreamStatus != StreamUpcoming {
		vd := ""
		if wp != nil && wp.Ytcfg != nil {
			vd = wp.Ytcfg.VisitorData
		}
		vi, err := p.fetchWithCookielessClient(ctx, videoID, vd, constants.VisionOSClient)
		level := st.noteVisionOS(vi, err)
		if err != nil || vi == nil {
			p.logger.Debug("[PlayerApi] JS-less ranking: visionos failed", "videoID", videoID, "error", errText(err))
		} else {
			stampManifestSources(vi, "visionos")
			var extra []Format
			collectFormats(&extra, vi.Formats, "visionos", level)
			info.Formats = MergeFormatPool(ctx, info.Formats, extra)
			info.FormatDiag.DRMSkipped, info.FormatDiag.CollapsedRenditions = st.poolCounts()
		}
	}
	if m := st.jsl.hls.Load(); m != nil && info.HlsManifestURL != "" &&
		cipher.HasNChallenge(info.HlsManifestURL) && !cipher.HasNChallenge(m.url) {
		info.HlsManifestURL, info.HlsManifestSource, info.HlsManifestAvailableAt = m.url, "visionos", m.at
	}
	p.logger.Debug("[PlayerApi] JS-less ranking: no player JS challenge can be solved, visionos ranked first",
		"videoID", videoID, "formats", len(info.Formats), "hlsSource", info.HlsManifestSource)
}

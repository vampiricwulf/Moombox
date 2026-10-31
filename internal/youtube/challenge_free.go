package youtube

import (
	"context"
	"errors"
	"fmt"

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

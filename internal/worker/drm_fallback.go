package worker

import (
	"errors"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// errDRMUnrecordable leads the error of a job whose every recordable source
// is DRM-protected (owner decision FE-1): the DASH manifest's
// Representations of a kind it needs carry ContentProtection and there is no
// HLS manifest to fall to, or the HLS playlist is encrypted too (the
// engine's refusal, engine.ErrDRMProtected). Nothing the operator changes —
// cookies, a retry, another quality — makes such a stream recordable, and
// the stored error says so first.
var errDRMUnrecordable = errors.New("DRM-protected; Moombox cannot record it")

// drmUnrecordable puts errDRMUnrecordable in front of an error that carries
// engine.ErrDRMProtected, once; any other error comes back as it is.
func drmUnrecordable(err error) error {
	if err == nil || !errors.Is(err, engine.ErrDRMProtected) || errors.Is(err, errDRMUnrecordable) {
		return err
	}
	return fmt.Errorf("%w: %w", errDRMUnrecordable, err)
}

// drmHlsFallback returns the strategy a DASH-manifest setup that failed on
// DRM falls to, or nil: HlsStrategy when the setup failed with
// engine.ErrDRMProtected — every Representation of a kind the job needs
// carries ContentProtection (engine.ParseDashDRM) — and the extraction lists
// an HLS manifest. yt-dlp drops the has_drm formats and takes whatever
// playable format remains (YoutubeDL.py, the has_drm filter in
// process_video_result; _video.py marks the formats), which for a stream
// whose DASH is protected is its HLS; so does this, as the live HLS
// fallbacks before it (missingPotFallback, the challenge arm) record the HLS
// playlist when the from-start setup cannot run. A VOD or post-live stream
// whose extraction lists an HLS manifest falls to it as well: DownloadHls
// records a finished playlist as readily as a live one. An HLS playlist that
// turns out encrypted too is refused by the engine before anything is
// written (runHlsLoop), and the job ends on errDRMUnrecordable.
func drmHlsFallback(strategy DownloadStrategy, info *youtube.VideoInfo, err error) DownloadStrategy {
	if !errors.Is(err, engine.ErrDRMProtected) || info == nil || info.HlsManifestURL == "" || strategy.Kind() != DashStrategy.Kind() {
		return nil
	}
	return HlsStrategy
}

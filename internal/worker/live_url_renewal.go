package worker

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// liveURLRenewal is the URL-expiry reader a manifest-free DASH downloader is
// built with (engine.DownloaderOptions.URLExpiry), which arms the engine's
// proactive renewal of its URL and token before googlevideo's expire= (audit
// G48): formatURLExpiry — the reader the VOD slot wait already uses — for a
// stream that is live at setup, nil for a post-live or VOD run. The renewal
// itself is OnProactiveRefresh, renewGvsCredentials without the 403
// recovery's cache invalidation.
//
// Live only, as yt-dlp's url_feed is: it re-extracts during a live download
// and hands a finished stream's generator none (url_feed if is_live else
// None, _video.py _prepare_live_from_start_formats). A post-live download
// fetches below a known head, where the 403 recovery's behind-head gate is
// open and renews an expired URL in place; at the live edge it never opens,
// which is the gap this closes.
func liveURLRenewal(info *youtube.VideoInfo) func(string) (time.Time, bool) {
	if info == nil || info.StreamStatus != youtube.StreamLive {
		return nil
	}
	return formatURLExpiry
}

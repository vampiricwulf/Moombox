package worker

import (
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// vodStreamID is the engine StreamID of one whole-file VOD stream: the video,
// the itag and the file's byte length (Format.ContentLength, the URL's clen).
// The staging names are fixed — every rendition lands in video.mp4 or
// audio.m4a — so the sidecar a run leaves is all that says WHICH file its
// bytes are a prefix of, and a resume after the selection moved (a changed
// max_video_resolution or prefer_60fps, a missing_pot degrade, a different
// format pool) must not append one rendition to another's checkpoint. The
// engine compares this before anything else (resumeIdentityMismatch) and
// starts a mismatched whole-file download over.
func vodStreamID(videoID string, f *youtube.Format) string {
	return fmt.Sprintf("%s/itag=%d/clen=%s", videoID, f.Itag, f.ContentLength)
}

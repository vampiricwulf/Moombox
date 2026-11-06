package worker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// displayAspectFile names the record beside a YouTube job's staged media of
// the display aspect its archive mux sets: the ratio a `yt:stretch=W:H`
// keyword asks for (youtube.VideoInfo.StretchedRatio). Old anamorphic
// uploads store 4:3 frames meant for 16:9 display; yt-dlp's default fixup
// remuxes them with `-aspect` (FFmpegFixupStretchedPP, YoutubeDL.py:3630-3633),
// and before this Moombox archived them squeezed.
//
// A file in staging rather than a field on the row: the mux that reads it
// can run in another process — a boot re-mux of an interrupted Muxing row,
// the operator's Mux action — and staging is what every one of those reads
// anyway; a column would need a schema migration for a keyword only pre-2012
// uploads carry. Written only for such a video: every other job's staging is
// exactly what it was.
const displayAspectFile = "display-aspect.txt"

// recordDisplayAspect writes, or clears, stagingDir's display-aspect record
// for ratio: written when ratio is a stretch (above 0 and not 1, the two
// values upstream's fixup skips), removed otherwise, so a re-run after the
// keyword was dropped muxes square again. The ratio is written as upstream
// passes it to FFmpeg, `%f`.
func recordDisplayAspect(stagingDir string, ratio float64) error {
	path := filepath.Join(stagingDir, displayAspectFile)
	if ratio <= 0 || ratio == 1 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatFloat(ratio, 'f', 6, 64)+"\n"), 0o644)
}

// stagedDisplayAspect is the display aspect recorded beside the staged
// video at videoPath — the record in the video's own directory — or "" for
// none, an unreadable one, or one that is not a positive number. The record
// lives in the job's staging root, so the archive's copy of a root
// recording and of a set-aside one beside it take it; a part's recording
// in its seg_N directory does not (a yt:stretch video is an old upload, a
// VOD, which records into the root).
func stagedDisplayAspect(videoPath string) string {
	if videoPath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(videoPath), displayAspectFile))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(data))
	if f, err := strconv.ParseFloat(v, 64); err != nil || f <= 0 {
		return ""
	}
	return v
}

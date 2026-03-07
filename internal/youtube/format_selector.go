package youtube

import (
	"fmt"
	"regexp"
	"strings"
)

var codecRegex = regexp.MustCompile(`codecs="?([^",]+)`)

// Video codec priority patterns (higher index = better).
var videoCodecPatterns = []struct {
	pattern *regexp.Regexp
	score   int
}{
	{regexp.MustCompile(`^vp9\.2`), 5},
	{regexp.MustCompile(`^vp9`), 4},
	{regexp.MustCompile(`^vp09`), 4},
	{regexp.MustCompile(`^av01`), 3},
	{regexp.MustCompile(`^avc1`), 2},
	{regexp.MustCompile(`^h264`), 1},
}

// Audio codec priority patterns.
var audioCodecPatterns = []struct {
	pattern *regexp.Regexp
	score   int
}{
	{regexp.MustCompile(`^opus`), 4},
	{regexp.MustCompile(`^mp4a\.40\.5`), 3},
	{regexp.MustCompile(`^mp4a\.40\.2`), 2},
	{regexp.MustCompile(`^mp4a`), 1},
}

// SelectedFormats contains the best video and audio formats.
type SelectedFormats struct {
	Video *Format
	Audio *Format
}

// SelectBestFormats selects the best video and audio formats based on configuration.
// Use SelectBestFormatsWithLogger to get debug logging of format selection.
func SelectBestFormats(formats []Format, maxResolution int, prefer60fps bool) SelectedFormats {
	return selectBestFormatsImpl(formats, maxResolution, prefer60fps, nil)
}

// SelectBestFormatsWithLogger selects formats with debug logging (matching TS formatSelector).
func SelectBestFormatsWithLogger(formats []Format, maxResolution int, prefer60fps bool, logger interface {
	Debug(msg string, args ...any)
}) SelectedFormats {
	return selectBestFormatsImpl(formats, maxResolution, prefer60fps, logger)
}

func selectBestFormatsImpl(formats []Format, maxResolution int, prefer60fps bool, logger interface {
	Debug(msg string, args ...any)
}) SelectedFormats {
	var bestVideo, bestAudio *Format
	var bestVideoCodecScore, bestAudioCodecScore int

	// Cache codec scores by mimeType to avoid redundant regex matching
	codecScoreCache := make(map[string]int)
	cachedVideoCodecScore := func(mimeType string) int {
		if score, ok := codecScoreCache[mimeType]; ok {
			return score
		}
		score := scoreVideoCodec(extractCodec(mimeType))
		codecScoreCache[mimeType] = score
		return score
	}
	cachedAudioCodecScore := func(mimeType string) int {
		key := "a:" + mimeType
		if score, ok := codecScoreCache[key]; ok {
			return score
		}
		score := scoreAudioCodec(extractCodec(mimeType))
		codecScoreCache[key] = score
		return score
	}

	// Log available video formats (matching TS FormatSelector debug output)
	if logger != nil {
		var available []string
		for i := range formats {
			f := &formats[i]
			if strings.Contains(f.MimeType, "video") && f.URL != "" {
				w, h, fps := 0, 0, 0
				if f.Width != nil {
					w = *f.Width
				}
				if f.Height != nil {
					h = *f.Height
				}
				if f.Fps != nil {
					fps = *f.Fps
				}
				available = append(available, fmt.Sprintf("itag %d (%dx%d@%dfps, %dbps)", f.Itag, w, h, fps, f.Bitrate))
			}
		}
		if len(available) > 0 {
			logger.Debug(fmt.Sprintf("[FormatSelector] Available video formats with URLs: %s", strings.Join(available, ", ")))
		}
	}

	for i := range formats {
		f := &formats[i]
		mimeType := f.MimeType

		if strings.Contains(mimeType, "video") {
			if f.URL == "" {
				continue
			}

			maxDim := f.MaxDimension()
			if maxDim > maxResolution {
				continue
			}

			if bestVideo == nil {
				bestVideo = f
				bestVideoCodecScore = cachedVideoCodecScore(f.MimeType)
				continue
			}

			bestMaxDim := bestVideo.MaxDimension()

			// Prefer higher resolution
			if maxDim > bestMaxDim {
				bestVideo = f
				bestVideoCodecScore = cachedVideoCodecScore(f.MimeType)
				continue
			}

			if maxDim == bestMaxDim {
				fFps := 30
				if f.Fps != nil {
					fFps = *f.Fps
				}
				bestFps := 30
				if bestVideo.Fps != nil {
					bestFps = *bestVideo.Fps
				}

				// FPS tiebreaker
				if fFps != bestFps {
					if prefer60fps && fFps > bestFps {
						bestVideo = f
					} else if !prefer60fps && fFps < bestFps {
						bestVideo = f
					}
					continue
				}

				// Codec score tiebreaker
				fCodec := cachedVideoCodecScore(f.MimeType)
				if fCodec != bestVideoCodecScore {
					if fCodec > bestVideoCodecScore {
						bestVideo = f
						bestVideoCodecScore = fCodec
					}
					continue
				}

				// Lower bitrate wins (better compression)
				if f.Bitrate < bestVideo.Bitrate {
					bestVideo = f
				} else if f.Bitrate == bestVideo.Bitrate {
					// Lower auth level wins
					fAuth := authLevelOf(f)
					bestAuth := authLevelOf(bestVideo)
					if fAuth < bestAuth {
						bestVideo = f
					}
				}
			}
		} else if strings.Contains(mimeType, "audio") {
			if f.URL == "" {
				continue
			}

			if bestAudio == nil {
				bestAudio = f
				bestAudioCodecScore = cachedAudioCodecScore(f.MimeType)
				continue
			}

			// Codec score
			fCodec := cachedAudioCodecScore(f.MimeType)
			if fCodec != bestAudioCodecScore {
				if fCodec > bestAudioCodecScore {
					bestAudio = f
					bestAudioCodecScore = fCodec
				}
				continue
			}

			// Higher bitrate wins
			if f.Bitrate > bestAudio.Bitrate {
				bestAudio = f
			} else if f.Bitrate == bestAudio.Bitrate {
				fAuth := authLevelOf(f)
				bestAuth := authLevelOf(bestAudio)
				if fAuth < bestAuth {
					bestAudio = f
				}
			}
		}
	}

	if logger != nil {
		if bestVideo != nil {
			w, h := 0, 0
			if bestVideo.Width != nil {
				w = *bestVideo.Width
			}
			if bestVideo.Height != nil {
				h = *bestVideo.Height
			}
			logger.Debug(fmt.Sprintf("[FormatSelector] best video: itag=%d %s %dx%d %dbps (source=%s)",
				bestVideo.Itag, extractCodec(bestVideo.MimeType),
				w, h,
				bestVideo.Bitrate, bestVideo.Source))
		} else {
			logger.Debug("[FormatSelector] no suitable video format found")
		}
		if bestAudio != nil {
			logger.Debug(fmt.Sprintf("[FormatSelector] best audio: itag=%d %s %dbps (source=%s)",
				bestAudio.Itag, extractCodec(bestAudio.MimeType),
				bestAudio.Bitrate, bestAudio.Source))
		} else {
			logger.Debug("[FormatSelector] no suitable audio format found")
		}
	}

	return SelectedFormats{Video: bestVideo, Audio: bestAudio}
}

// SelectWithOptions selects formats with optional manual itag override.
func SelectWithOptions(formats []Format, maxResolution int, prefer60fps bool, videoItag, audioItag *int) SelectedFormats {
	var video, audio *Format

	// Manual video selection
	if videoItag != nil && *videoItag != -1 {
		for i := range formats {
			if formats[i].Itag == *videoItag && strings.Contains(formats[i].MimeType, "video") && formats[i].URL != "" {
				video = &formats[i]
				break
			}
		}
	}

	// Manual audio selection
	if audioItag != nil && *audioItag != -1 {
		for i := range formats {
			if formats[i].Itag == *audioItag && strings.Contains(formats[i].MimeType, "audio") && formats[i].URL != "" {
				audio = &formats[i]
				break
			}
		}
	}

	// Explicit skip
	skipVideo := videoItag != nil && *videoItag == -1
	skipAudio := audioItag != nil && *audioItag == -1

	// Auto-select for unresolved
	auto := SelectBestFormats(formats, maxResolution, prefer60fps)
	if video == nil && !skipVideo {
		video = auto.Video
	}
	if audio == nil && !skipAudio {
		audio = auto.Audio
	}

	return SelectedFormats{Video: video, Audio: audio}
}

func extractCodec(mimeType string) string {
	m := codecRegex.FindStringSubmatch(mimeType)
	if m == nil {
		return ""
	}
	return m[1]
}

func scoreVideoCodec(codec string) int {
	for _, p := range videoCodecPatterns {
		if p.pattern.MatchString(codec) {
			return p.score
		}
	}
	return 0
}

func scoreAudioCodec(codec string) int {
	for _, p := range audioCodecPatterns {
		if p.pattern.MatchString(codec) {
			return p.score
		}
	}
	return 0
}

func authLevelOf(f *Format) int {
	if f.AuthLevel != nil {
		return *f.AuthLevel
	}
	return 999
}

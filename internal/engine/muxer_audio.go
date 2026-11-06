package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoAudioTrack is ExtractAudioCopy's answer for an input with no audio
// stream to keep: FFmpeg refuses the `-map 0:a` with "Stream map '0:a'
// matches no streams", and nothing is written.
var ErrNoAudioTrack = errors.New("the input has no audio track")

// ExtractAudioCopy writes the audio of inputPath alone to outputPath by
// stream copy — `-map 0:a -c copy`, every audio stream and no other, not a
// sample re-encoded — the way every other FFmpeg write goes (writeOutput):
// into a temporary name beside outputPath, the checks run on it there, and
// renamed into place only once FFmpeg has exited 0 and every check passed.
// outputPath's extension picks the container (an .m4a is MP4 audio, which
// takes the AAC a Twitch clip carries).
//
// It is how an audio_only clip job keeps only its rendition's audio (owner
// decision FQ-4, internal/worker's twitchClipExtractAudio): Twitch serves a
// clip as whole MP4s with video, and none without. An input with no audio
// stream is ErrNoAudioTrack, wrapped with FFmpeg's own words.
func (m *Muxer) ExtractAudioCopy(ctx context.Context, inputPath, outputPath string, checks ...OutputCheck) error {
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("input file not found: %w", err)
	}
	if dir := filepath.Dir(outputPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	return m.writeOutput(ctx, outputPath, checks, func(tmp string) error {
		args := []string{"-y", "-i", ffmpegPathArg(inputPath), "-map", "0:a", "-c", "copy", "-movflags", "faststart", ffmpegPathArg(tmp)}
		m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
		err := m.runFFmpeg(ctx, args)
		if err != nil && ctx.Err() == nil && strings.Contains(err.Error(), "matches no streams") {
			return fmt.Errorf("%w: %w", ErrNoAudioTrack, err)
		}
		return err
	})
}

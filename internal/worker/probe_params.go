package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// streamParams captures the codec/geometry parameters of a media file's
// primary video and audio streams, as reported by ffprobe. Used by the
// Tier 4 lossless same-format part merge to decide whether two parts can be
// concatenated with `-c copy` (identical params) or require a re-encode.
//
// FrameRate and SampleRate are kept as ffprobe's raw strings (e.g. "30/1",
// "44100") rather than parsed to float/int — string equality is exactly
// what merge-compatibility needs and avoids float traps like 29.97 vs
// 30000/1001 comparing unequal after lossy conversion.
type streamParams struct {
	VCodec     string
	Width      int
	Height     int
	FrameRate  string
	ACodec     string
	SampleRate string
	Channels   int
}

// equal reports whether p and q describe merge-compatible streams. A nil
// receiver or nil argument is never equal to anything, including another
// nil — callers must have two successfully-probed streamParams before
// comparing.
func (p *streamParams) equal(q *streamParams) bool {
	if p == nil || q == nil {
		return false
	}
	return p.VCodec == q.VCodec &&
		p.Width == q.Width &&
		p.Height == q.Height &&
		p.FrameRate == q.FrameRate &&
		p.ACodec == q.ACodec &&
		p.SampleRate == q.SampleRate &&
		p.Channels == q.Channels
}

// probeStreamParams probes filePath's primary video and audio streams via
// ffprobe. Unlike probeAudioBitrate (which falls back to a default when
// metadata is missing), a probe failure or a missing video stream is
// returned as an error, never faked with a zero-value default — the Tier 4
// merge must abort on unknown stream params rather than silently attempt a
// copy-mode concat that could produce a corrupt or garbled output.
func probeStreamParams(ctx context.Context, ffprobePath, filePath string) (*streamParams, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	if len(output) == 0 {
		return nil, fmt.Errorf("ffprobe: empty output for %s", filePath)
	}

	var data struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &data); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	params := &streamParams{}
	haveVideo := false
	haveAudio := false
	for _, s := range data.Streams {
		switch s.CodecType {
		case "video":
			if haveVideo {
				continue // keep the first video stream only
			}
			params.VCodec = s.CodecName
			params.Width = s.Width
			params.Height = s.Height
			params.FrameRate = s.RFrameRate
			haveVideo = true
		case "audio":
			if haveAudio {
				continue // keep the first audio stream only
			}
			params.ACodec = s.CodecName
			params.SampleRate = s.SampleRate
			params.Channels = s.Channels
			haveAudio = true
		}
	}

	if !haveVideo {
		return nil, fmt.Errorf("ffprobe: no video stream found in %s", filePath)
	}

	return params, nil
}

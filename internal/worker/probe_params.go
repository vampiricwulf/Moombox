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
//
// VProfile/PixFmt/AProfile/ASampleFmt are also kept as ffprobe's raw
// strings, empty-string tolerant (some inputs/codecs don't report a
// profile). They exist because VCodec/Width/Height/FrameRate/ACodec/
// SampleRate/Channels alone are not sufficient to guarantee a safe -c copy
// concat: H.264 High profile and High 4:4:4 Predictive both probe as
// codec_name "h264" at the same dimensions/frame rate, but carry
// pix_fmt yuv420p vs yuv444p respectively — concatenating them with -c
// copy today probes "equal", succeeds, and produces an internally
// inconsistent file (differing chroma subsampling mid-stream), after which
// cleanup deletes the intact original parts. Comparing profile and pix_fmt
// (video) / profile and sample_fmt (audio) closes that gap.
type streamParams struct {
	VCodec     string
	Width      int
	Height     int
	FrameRate  string
	VProfile   string
	PixFmt     string
	ACodec     string
	SampleRate string
	Channels   int
	AProfile   string
	ASampleFmt string
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
		p.VProfile == q.VProfile &&
		p.PixFmt == q.PixFmt &&
		p.ACodec == q.ACodec &&
		p.SampleRate == q.SampleRate &&
		p.Channels == q.Channels &&
		p.AProfile == q.AProfile &&
		p.ASampleFmt == q.ASampleFmt
}

// probeStreamParamsFn is mergeSameFormatParts' probe implementation,
// indirected through a package var so tests can substitute a spy (to prove,
// e.g., that a platform gate skips probing entirely) without a real ffprobe
// toolchain. Production code never reassigns this.
var probeStreamParamsFn = probeStreamParams

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
			Profile    string `json:"profile"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
			PixFmt     string `json:"pix_fmt"`
			SampleRate string `json:"sample_rate"`
			SampleFmt  string `json:"sample_fmt"`
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
			params.VProfile = s.Profile
			params.PixFmt = s.PixFmt
			haveVideo = true
		case "audio":
			if haveAudio {
				continue // keep the first audio stream only
			}
			params.ACodec = s.CodecName
			params.SampleRate = s.SampleRate
			params.Channels = s.Channels
			params.AProfile = s.Profile
			params.ASampleFmt = s.SampleFmt
			haveAudio = true
		}
	}

	if !haveVideo {
		return nil, fmt.Errorf("ffprobe: no video stream found in %s", filePath)
	}

	return params, nil
}

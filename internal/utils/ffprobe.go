package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// VideoMetadata contains metadata extracted from a video file via ffprobe.
type VideoMetadata struct {
	Duration   float64 `json:"duration"`
	VideoCodec string  `json:"videoCodec,omitempty"`
	AudioCodec string  `json:"audioCodec,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	Fps        float64 `json:"fps,omitempty"`
	FileSize   int64   `json:"fileSize,omitempty"`
}

type ffprobeOutput struct {
	Format struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
	} `json:"streams"`
}

// ExtractVideoMetadata extracts metadata from a video file using ffprobe.
// If ffprobePath is empty, "ffprobe" is used (resolved via PATH).
func ExtractVideoMetadata(ctx context.Context, ffprobePath, filePath string) (*VideoMetadata, error) {
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
	}

	var probe ffprobeOutput
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	meta := &VideoMetadata{}

	// Parse duration
	if probe.Format.Duration != "" {
		meta.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	}

	// Parse file size
	if probe.Format.Size != "" {
		meta.FileSize, _ = strconv.ParseInt(probe.Format.Size, 10, 64)
	}

	// Extract stream info
	for _, stream := range probe.Streams {
		switch stream.CodecType {
		case "video":
			meta.VideoCodec = stream.CodecName
			meta.Width = stream.Width
			meta.Height = stream.Height
			meta.Fps = parseFrameRate(stream.RFrameRate)
		case "audio":
			meta.AudioCodec = stream.CodecName
		}
	}

	return meta, nil
}

func parseFrameRate(rate string) float64 {
	// Format is typically "30/1" or "30000/1001"
	var num, den float64
	n, _ := fmt.Sscanf(rate, "%f/%f", &num, &den)
	if n == 2 && den > 0 {
		return num / den
	}
	f, _ := strconv.ParseFloat(rate, 64)
	return f
}

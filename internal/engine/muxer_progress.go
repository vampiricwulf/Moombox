package engine

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// runFFmpegWithProgress runs FFmpeg, parsing stderr for time= progress lines.
// progressFn is called with 0-100 based on currentTime/totalDuration.
func (m *Muxer) runFFmpegWithProgress(ctx context.Context, args []string, totalDuration float64, progressFn func(float64)) error {
	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)
	cmd.Stdout = nil

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	// Read stderr fully before cmd.Wait() per Go docs
	var lastErr string
	scanner := bufio.NewScanner(stderr)
	scanner.Split(scanFFmpegLines)
	for scanner.Scan() {
		line := scanner.Text()
		if t := parseFFmpegTime(line); t > 0 && totalDuration > 0 {
			pct := min((t/totalDuration)*100, 100)
			progressFn(pct)
		}
		// Keep tail of stderr for error reporting
		if strings.TrimSpace(line) != "" {
			lastErr = line
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg timed out: %w", ctx.Err())
		}
		m.logger.Error("ffmpeg failed", "stderr", lastErr)
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", err, lastErr)
	}

	progressFn(100)
	return nil
}

// scanFFmpegLines is a bufio.SplitFunc that splits on \r or \n.
// FFmpeg writes progress lines with \r (carriage return) on the same line.
func scanFFmpegLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseFFmpegTime extracts the time in seconds from an FFmpeg progress line.
// Looks for "time=HH:MM:SS.xx" or "time=SS.xx" patterns.
func parseFFmpegTime(line string) float64 {
	_, rest, ok := strings.Cut(line, "time=")
	if !ok {
		return 0
	}
	// Find end of time value (space or end of string)
	timeStr, _, _ := strings.Cut(rest, " ")
	timeStr = strings.TrimSpace(timeStr)
	if timeStr == "" || timeStr == "N/A" {
		return 0
	}

	// Try HH:MM:SS.xx format
	parts := strings.Split(timeStr, ":")
	switch len(parts) {
	case 3:
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		s, _ := strconv.ParseFloat(parts[2], 64)
		return h*3600 + m*60 + s
	case 2:
		m, _ := strconv.ParseFloat(parts[0], 64)
		s, _ := strconv.ParseFloat(parts[1], 64)
		return m*60 + s
	default:
		s, _ := strconv.ParseFloat(timeStr, 64)
		return s
	}
}

// cappedBuffer is an io.Writer that caps its size. When the buffer exceeds
// maxSize, it truncates to keep only the last keepSize bytes.
type cappedBuffer struct {
	buf      []byte
	maxSize  int
	keepSize int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.maxSize {
		// Copy to a new slice so the old backing array can be GC'd
		tail := c.buf[len(c.buf)-c.keepSize:]
		c.buf = make([]byte, len(tail))
		copy(c.buf, tail)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return string(c.buf)
}

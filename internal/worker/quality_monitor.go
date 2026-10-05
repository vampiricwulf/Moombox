package worker

import (
	"context"
	"sync"
	"time"
)

// QualityMonitor periodically probes stream quality and detects changes.
// It runs on a fixed interval (typically 30s) and sends new quality info
// to a channel when the quality differs from the current baseline.
type QualityMonitor struct {
	interval time.Duration
	mu       sync.Mutex
	current  QualityInfo
	probeFn  func(ctx context.Context) (*QualityInfo, error)
	logger   logger

	// lastSignaled is the probed quality the last change signal carried;
	// strikeQuality and strikes count how many times running that same
	// quality was signalled and answered "unchanged" (ReconcileSameQuality).
	lastSignaled  QualityInfo
	strikeQuality QualityInfo
	strikes       int
}

// sameQualityStrikeLimit is how many consecutive refreshes may answer the same
// probed quality with "the download is unchanged" before the monitor takes the
// probe's reading as its baseline — see ReconcileSameQuality.
const sameQualityStrikeLimit = 3

// NewQualityMonitor creates a quality monitor.
func NewQualityMonitor(interval time.Duration, current QualityInfo, probeFn func(ctx context.Context) (*QualityInfo, error), logger logger) *QualityMonitor {
	return &QualityMonitor{
		interval: interval,
		current:  current,
		probeFn:  probeFn,
		logger:   logger,
	}
}

// Run polls for quality changes until ctx is cancelled.
// When a change is detected, the new quality is sent to changeCh.
// Probe errors are logged and skipped (never trigger false positives).
func (m *QualityMonitor) Run(ctx context.Context, changeCh chan<- QualityInfo) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("panic in quality monitor", "panic", r)
		}
	}()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probed, err := m.probeFn(ctx)
			if err != nil {
				m.logger.Debug("quality probe error (skipping)", "err", err)
				continue
			}
			if probed == nil {
				continue
			}
			m.mu.Lock()
			changed := m.current.Changed(*probed)
			if changed {
				m.logger.Info("quality change detected",
					"from", m.current.Label, "to", probed.Label,
					"fromRes", formatRes(m.current), "toRes", formatRes(*probed))
				m.current = *probed
				m.lastSignaled = *probed
			}
			m.mu.Unlock()
			if changed {
				select {
				case changeCh <- *probed:
				default:
					// Channel full — previous change not yet consumed
				}
			}
		}
	}
}

// UpdateBaseline updates the monitor's current quality without triggering a change.
// Safe to call from any goroutine.
func (m *QualityMonitor) UpdateBaseline(q QualityInfo) {
	m.mu.Lock()
	m.current = q
	m.strikes = 0
	m.mu.Unlock()
}

// ReconcileSameQuality is the loop's answer to a change signal whose refresh
// came back at the download's own quality. Normally it re-baselines to that
// quality, as UpdateBaseline does, so a probe that reports the signalled
// quality again signals again — the refresh may simply have run before the
// new rendition reached the manifest. But when the SAME probed quality has
// been signalled and answered "unchanged" sameQualityStrikeLimit times
// running, the probe and the download disagree for good — an HLS download
// beside the probe's DASH ladder, a top rung the probe's client sees and the
// downloader's does not — and re-baselining to the download's quality made
// every 30 s tick cancel and rebuild the downloaders for the rest of the
// broadcast. The baseline then becomes the probed quality instead, so the
// monitor stays quiet until the probe itself reports something else. It
// reports whether it settled that way.
func (m *QualityMonitor) ReconcileSameQuality(download QualityInfo) (settled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.strikes > 0 && !m.lastSignaled.Changed(m.strikeQuality) {
		m.strikes++
	} else {
		m.strikeQuality, m.strikes = m.lastSignaled, 1
	}
	if m.strikes >= sameQualityStrikeLimit {
		m.current = m.lastSignaled
		m.strikes = 0
		m.logger.Warn("the quality probe and the download disagree persistently; following the download until the probe reports something new",
			"probe", m.lastSignaled.Label, "download", download.Label)
		return true
	}
	m.current = download
	return false
}

func formatRes(q QualityInfo) string {
	return FormatQualityLabel(q.Height, q.FPS)
}

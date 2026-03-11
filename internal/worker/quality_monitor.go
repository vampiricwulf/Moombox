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
}

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
	m.mu.Unlock()
}

func formatRes(q QualityInfo) string {
	return FormatQualityLabel(q.Height, q.FPS)
}

package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// levelRecordingLogger records "<level> <msg>" for the feed-check lines only.
type levelRecordingLogger struct{ lines []string }

func (l *levelRecordingLogger) record(level, msg string) {
	switch msg {
	case "feed check failed", "feed check still failing", "feed check recovered":
		l.lines = append(l.lines, level+" "+msg)
	}
}
func (l *levelRecordingLogger) Debug(msg string, args ...any) { l.record("debug", msg) }
func (l *levelRecordingLogger) Info(msg string, args ...any)  { l.record("info", msg) }
func (l *levelRecordingLogger) Warn(msg string, args ...any)  { l.record("warn", msg) }
func (l *levelRecordingLogger) Error(msg string, args ...any) { l.record("error", msg) }

// A failing feed warns once when the failures start, logs each repeat at
// Debug, and says once at Info that the channel answers again. A channel that
// was never failing logs nothing on success, and a new streak warns again.
// The health tracker still counts every failure.
//
// Mutants: warning on every failure (three warns in the first streak); no
// recovery line; a recovery line on every success (two infos); the streak not
// resetting (the second streak logs Debug instead of Warn).
func TestFeed_FailureWarnsOncePerStreak(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	fail := false
	fm := newTestFeedMonitor(t, db,
		withRSS(func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
			if fail {
				return nil, feedStatusError(404)
			}
			return rssXML(), nil
		}),
		withProbe(stubProbeErrored()),
	)
	log := &levelRecordingLogger{}
	fm.logger = log
	setChannels(fm, chYT("UC1"))
	cycle := func(failing bool) {
		fail = failing
		fm.doCheck(context.Background())
	}

	cycle(false)
	if len(log.lines) != 0 {
		t.Fatalf("a healthy channel logged %q", log.lines)
	}
	cycle(true)
	cycle(true)
	cycle(true)
	if n := fm.health.snapshot()[0].ConsecutiveErrors; n != 3 {
		t.Errorf("ConsecutiveErrors = %d, want 3: the tracker counts every failure", n)
	}
	cycle(false)
	cycle(false)
	cycle(true)
	want := []string{
		"warn feed check failed",
		"debug feed check still failing",
		"debug feed check still failing",
		"info feed check recovered",
		"warn feed check failed",
	}
	if !slices.Equal(log.lines, want) {
		t.Errorf("feed-check log lines = %q, want %q", log.lines, want)
	}
}

// A feed 404 says in plain words that YouTube's feed does this when it is
// busy, that it usually clears by itself and that the channel is still
// checked (owner ruling R2-M3). The same text is the health entry's LastError,
// which both Settings channel lists and the Channel Not Responding alert show.
// The TUI list shows the first 48 cells of it, so "YouTube is busy" has to sit
// inside them. Every other status keeps its bare text.
func TestFeed_404SaysYouTubeIsBusy(t *testing.T) {
	msg := feedStatusError(404).Error()
	for _, want := range []string{"feed http 404", "YouTube is busy", "usually clears by itself", "the channel is still checked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("404 text %q does not say %q", msg, want)
		}
	}
	if end := strings.Index(msg, "YouTube is busy") + len("YouTube is busy"); end > 47 {
		t.Errorf("\"YouTube is busy\" ends at cell %d of %q: the TUI's 48-cell cut hides it", end, msg)
	}
	for _, status := range []int{403, 429, 500, 503} {
		if got, want := feedStatusError(status).Error(), fmt.Sprintf("feed http %d", status); got != want {
			t.Errorf("status %d reads %q, want %q", status, got, want)
		}
	}

	shrinkFeedStagger(t)
	fm := newTestFeedMonitor(t, newTestDB(t), withRSS(rss404()), withProbe(stubProbeErrored()))
	setChannels(fm, chYT("UC1"))
	fm.doCheck(context.Background())
	if got := fm.health.snapshot()[0].LastError; !strings.Contains(got, feedBusy404Note) {
		t.Errorf("LastError = %q, want it to carry the 404 note", got)
	}
}

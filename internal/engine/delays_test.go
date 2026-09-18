package engine

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDefaultDelaysMatchConstants pins production timing: every loop wait the
// downloader sleeps on equals the constant that documented it before the
// delays struct existed, and NewSegmentDownloader installs exactly those
// defaults. A new field without a default is caught by the zero check.
func TestDefaultDelaysMatchConstants(t *testing.T) {
	want := delays{
		singleGoneRetry:        singleGoneRetryDelay,
		interruptionStallRetry: interruptionStallRetryDelay,
		transientFailureRetry:  transientFailureRetryDelay,
		genericRetry:           genericRetryDelay,
		hlsPlaylistRetry:       hlsPlaylistRetryDelay,
		hlsStuckRetry:          hlsStuckRetryDelay,
		connectivityPoll:       connectivityPollInterval,
		atEdgeBackoffUnit:      time.Second,
		hlsReloadUnit:          time.Second,
		hlsResumeSave:          hlsResumeSaveInterval,
		fetchHardCeiling:       segmentHardCeiling,
	}
	if got := defaultDelays(); got != want {
		t.Fatalf("defaultDelays() = %+v, want %+v", got, want)
	}
	// The literal values, so a constant edit is a visible diff here too.
	for name, pair := range map[string][2]time.Duration{
		"singleGoneRetry":        {want.singleGoneRetry, 500 * time.Millisecond},
		"interruptionStallRetry": {want.interruptionStallRetry, 5 * time.Second},
		"transientFailureRetry":  {want.transientFailureRetry, time.Second},
		"genericRetry":           {want.genericRetry, 2 * time.Second},
		"hlsPlaylistRetry":       {want.hlsPlaylistRetry, 5 * time.Second},
		"hlsStuckRetry":          {want.hlsStuckRetry, 2 * time.Second},
		"connectivityPoll":       {want.connectivityPoll, 5 * time.Second},
		"hlsResumeSave":          {want.hlsResumeSave, 15 * time.Second},
		"fetchHardCeiling":       {want.fetchHardCeiling, 15 * time.Minute},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want %v (production timing must not move in this arc)", name, pair[0], pair[1])
		}
	}
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: t.TempDir() + "/o.ts"})
	if d.delays != want {
		t.Errorf("NewSegmentDownloader installed %+v, want the defaults", d.delays)
	}
	v := reflect.ValueOf(want)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Int() == 0 {
			t.Errorf("delays.%s has no default", v.Type().Field(i).Name)
		}
	}
}

// fastScale is how much faster the test loops run than production. One
// factor for every wait, so escalation counts (ten singleGone retries before
// the stall arm, say) and orderings are exactly production's.
const fastScale = 20

// fastDelays is defaultDelays() ÷ fastScale: 500 ms → 25 ms, 5 s → 250 ms.
func fastDelays() delays {
	d := defaultDelays()
	return delays{
		singleGoneRetry:        d.singleGoneRetry / fastScale,
		interruptionStallRetry: d.interruptionStallRetry / fastScale,
		transientFailureRetry:  d.transientFailureRetry / fastScale,
		genericRetry:           d.genericRetry / fastScale,
		hlsPlaylistRetry:       d.hlsPlaylistRetry / fastScale,
		hlsStuckRetry:          d.hlsStuckRetry / fastScale,
		connectivityPoll:       d.connectivityPoll / fastScale,
		atEdgeBackoffUnit:      d.atEdgeBackoffUnit / fastScale,
		hlsReloadUnit:          d.hlsReloadUnit / fastScale,
		hlsResumeSave:          d.hlsResumeSave / fastScale,
		fetchHardCeiling:       d.fetchHardCeiling / fastScale,
	}
}

// fast scales a test's own timing knob (a MaxTimeout, a ceiling) by the same
// factor the loop waits were scaled by, so the knob keeps its relationship to
// the loop.
func fast(d time.Duration) time.Duration { return d / fastScale }

// activityRecorder wires OnActivity to a buffered channel so a test can wait
// for the loop to REACH a state instead of sleeping a wall-clock margin and
// hoping. Non-blocking send: a test that stops reading never stalls the loop.
func activityRecorder(d *SegmentDownloader) <-chan DownloadActivity {
	ch := make(chan DownloadActivity, 1024)
	d.OnActivity = func(a DownloadActivity) {
		select {
		case ch <- a:
		default:
		}
	}
	return ch
}

// awaitActivity blocks until want is observed on ch or within elapses.
func awaitActivity(t *testing.T, ch <-chan DownloadActivity, want DownloadActivity, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case a := <-ch:
			if a == want {
				return
			}
		case <-deadline:
			t.Fatalf("downloader never reported %v within %v", want, within)
		}
	}
}

func TestFastDelaysKeepRatios(t *testing.T) {
	f, p := fastDelays(), defaultDelays()
	if f.interruptionStallRetry/f.singleGoneRetry != p.interruptionStallRetry/p.singleGoneRetry {
		t.Fatalf("stall:singleGone ratio changed: fast %v:%v, prod %v:%v", f.interruptionStallRetry, f.singleGoneRetry, p.interruptionStallRetry, p.singleGoneRetry)
	}
	if f.singleGoneRetry < 20*time.Millisecond {
		t.Fatalf("singleGoneRetry %v is below the 20 ms floor timer jitter makes unsafe", f.singleGoneRetry)
	}
}

// TestDownloadActivityStringsAreDistinct pins DownloadActivity's String():
// awaitActivity and every %v on an activity must name the state, not print
// an integer. Iterates ActivityNone..ActivityWaitingResume; a member
// appended after ActivityWaitingResume is NOT covered — extend the upper
// bound when one is added.
func TestDownloadActivityStringsAreDistinct(t *testing.T) {
	seen := map[string]DownloadActivity{}
	for a := ActivityNone; a <= ActivityWaitingResume; a++ {
		got := a.String()
		if got == "" {
			t.Errorf("DownloadActivity(%d).String() is empty", int(a))
			continue
		}
		if !strings.HasPrefix(got, "Activity") {
			t.Errorf("DownloadActivity(%d).String() = %q, want the Activity<Name> constant name", int(a), got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("DownloadActivity(%d) and (%d) both stringify to %q", int(prev), int(a), got)
		}
		seen[got] = a
	}
	if got, want := DownloadActivity(99).String(), "DownloadActivity(99)"; got != want {
		t.Errorf("unknown activity String() = %q, want %q", got, want)
	}
}

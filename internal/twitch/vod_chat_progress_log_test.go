package twitch

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func progressLines(l *entryLogger) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logEntry
	for _, e := range l.entries {
		if e.level == "INFO" && e.msg == "VOD chat progress" {
			out = append(out, e)
		}
	}
	return out
}

// The "VOD chat progress" Info line is written about every 30 s, however
// many pages arrive in between, while the progress callback still fires on
// every page: the job row's chat count stays real-time, the log does not
// fill with thousands of near-identical lines.
func TestVodChatProgress_LogLineIsThrottledCallbackIsNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &entryLogger{}
		vcd := NewVodChatDownloader(nil, VodChatOptions{
			VodID:       "2345678901",
			VodDuration: 3600,
		}, log)
		var calls int
		vcd.SetOnProgress(func(int) { calls++ })
		vcd.lastProgressLog = time.Now() // as Start sets it

		// 10 pages a second for 95 s: 950 pages.
		const pages = 950
		for i := range pages {
			time.Sleep(100 * time.Millisecond)
			vcd.totalCount.Add(50)
			vcd.reportProgress(float64(i))
		}

		if calls != pages {
			t.Fatalf("progress callback fired %d times for %d pages, want every page", calls, pages)
		}
		lines := progressLines(log)
		if len(lines) != 3 { // at 30 s, 60 s and 90 s
			t.Fatalf("%d \"VOD chat progress\" lines in 95 s, want 3", len(lines))
		}
		for _, e := range lines {
			i := slices.Index(e.args, any("vodID"))
			if i < 0 || i+1 >= len(e.args) || e.args[i+1] != "2345678901" {
				t.Fatalf("progress line does not name its VOD: %v", e.args)
			}
		}
	})
}

// With no known duration there is no percentage to report and no line, as
// before; the callback still fires.
func TestVodChatProgress_UnknownDurationLogsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &entryLogger{}
		vcd := NewVodChatDownloader(nil, VodChatOptions{VodID: "1"}, log)
		var calls int
		vcd.SetOnProgress(func(int) { calls++ })
		for range 5 {
			time.Sleep(time.Minute)
			vcd.reportProgress(10)
		}
		if calls != 5 || len(progressLines(log)) != 0 {
			t.Fatalf("calls = %d, progress lines = %d; want 5 and 0", calls, len(progressLines(log)))
		}
	})
}

package chat

import "testing"

// Truth table (spec "Signals"): open on live polls returning a
// continuation; closed on a definitive IsComplete/empty-continuation that
// recovery does not rescue; UNCHANGED on fetch errors; never open for
// replay or a downloader that never started.
func TestLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused"})
	if cd.LiveContinuationOpen() {
		t.Fatal("never-started downloader must not report open")
	}
	cd.setLiveContinuationOpen(true)
	if !cd.LiveContinuationOpen() {
		t.Fatal("open after successful live poll")
	}
	cd.setLiveContinuationOpen(false)
	if cd.LiveContinuationOpen() {
		t.Fatal("closed after definitive end")
	}
}

func TestLiveContinuationOpenReplayNeverOpens(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused", IsReplay: true})
	cd.noteLivePollResult(true) // even a "successful poll" on replay
	if cd.LiveContinuationOpen() {
		t.Fatal("replay chat must never report live-open")
	}
}

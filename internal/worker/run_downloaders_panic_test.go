package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// A panic inside a downloader ran on one of runDownloaders' own goroutines,
// where no caller's recover reaches: it crashed the process, and every other
// job's capture with it. It now comes back as this download's error.
//
// The panic is raised from OnFinish, which Start's deferred cleanup calls on
// every exit.
//
// Mutant: the recover in runDownloaders removed — the test binary crashes.
func TestADownloaderPanicIsThisDownloadsError(t *testing.T) {
	d := engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL:    "http://127.0.0.1:1/never.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "missing", "dir", "video.ts"),
		IsHls:      true,
		StartSeq:   -1,
	})
	d.OnFinish = func() { panic("boom") }

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // whatever Start does first, it stops at once
	o := &DownloadOrchestrator{}
	err := o.runDownloaders(ctx, &DownloadResult{VideoDownloader: d})
	if err == nil || !strings.Contains(err.Error(), "video downloader panic: boom") {
		t.Fatalf("runDownloaders = %v, want the panic as an error", err)
	}
}

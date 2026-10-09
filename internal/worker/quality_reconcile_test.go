package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

func hasLogPrefix(logs *captureLogger, prefix string) bool {
	for _, m := range logs.lines() {
		if msg, _ := m[0].(string); strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// A probe and a download that disagree for good — an HLS download beside the
// probe's DASH ladder, a top rung only the probe's client sees — made every
// 30 s tick signal a change, cancel the downloaders and rebuild them at the
// same quality, for the rest of the broadcast: the same-quality answer re-
// baselined to the download's quality, which the probe never reports. Early
// refreshes still retry; the third "unchanged" answer to the SAME probed
// quality takes the probe's reading as the baseline, and a split (or any
// UpdateBaseline) starts the count again.
//
// Mutant: ReconcileSameQuality re-baselining to the download every time, a
// different probed quality not restarting the count, or UpdateBaseline not
// clearing it.
func TestQualityMonitorSettlesAPersistentDisagreement(t *testing.T) {
	dl := QualityInfo{Width: 1920, Height: 1080, FPS: 60, Label: "1080p60"}
	probe := QualityInfo{Width: 2560, Height: 1440, FPS: 60, Label: "1440p60"}
	other := QualityInfo{Width: 3840, Height: 2160, FPS: 60, Label: "2160p60"}
	m := NewQualityMonitor(0, dl, nil, discardLogger{})
	signal := func(q QualityInfo) { m.mu.Lock(); m.lastSignaled = q; m.current = q; m.mu.Unlock() }

	for i := 1; i < sameQualityStrikeLimit; i++ {
		signal(probe)
		if m.ReconcileSameQuality(dl) {
			t.Fatalf("settled on answer %d, before the limit", i)
		}
		if m.current.Changed(dl) {
			t.Fatalf("answer %d: baseline %v, want the download's quality back", i, m.current)
		}
	}
	signal(probe)
	if !m.ReconcileSameQuality(dl) || m.current.Changed(probe) {
		t.Fatalf("the limit-th answer to the same probe: baseline %v, want the probe's %v", m.current, probe)
	}

	// A different probed quality starts its own count.
	signal(other)
	if m.ReconcileSameQuality(dl) {
		t.Error("a new probed quality settled on its first answer")
	}
	for i := 1; i < sameQualityStrikeLimit; i++ {
		signal(probe)
		if m.ReconcileSameQuality(dl) {
			t.Errorf("answer %d to the probe after one to another quality settled — the count carried over", i)
		}
	}
	m.UpdateBaseline(dl)
	for i := 1; i < sameQualityStrikeLimit; i++ {
		signal(probe)
		if m.ReconcileSameQuality(dl) {
			t.Fatalf("after UpdateBaseline, settled on answer %d", i)
		}
	}
}

// The quality Run signals is the one ReconcileSameQuality counts against.
//
// Mutant: Run not recording lastSignaled.
func TestQualityMonitorRecordsWhatItSignals(t *testing.T) {
	dl := QualityInfo{Width: 1920, Height: 1080, FPS: 60, Label: "1080p60"}
	probe := QualityInfo{Width: 2560, Height: 1440, FPS: 60, Label: "1440p60"}
	m := NewQualityMonitor(time.Millisecond, dl, func(context.Context) (*QualityInfo, error) { p := probe; return &p, nil }, discardLogger{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan QualityInfo, 1)
	go m.Run(ctx, ch)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no change signalled")
	}
	cancel()
	m.mu.Lock()
	got := m.lastSignaled
	m.mu.Unlock()
	if got.Changed(probe) {
		t.Errorf("lastSignaled = %v, want the signalled %v", got, probe)
	}
}

// The live loop's same-quality continue is what calls it.
//
// Mutant: the same-quality path calling UpdateBaseline again.
func TestLiveLoopReconcilesSameQualityRefreshes(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "monitor.ReconcileSameQuality(currentQuality)") {
		t.Error("runLiveStreamDownload's same-quality path no longer reconciles with the monitor")
	}
}

// A manual itag the live selection could not honour used to fall back to
// automatic selection without a word on the DASH and manifest-free paths
// (the VOD path warns), and an HLS stream ignored any pin silently.
//
// Mutant: dropping warnUnhonouredPins (or either half), or the HLS warning.
func TestUnhonouredPinsAreWarned(t *testing.T) {
	job, logs := vodPotJob(t)
	pin := 303
	job.Job.SelectedVideoItag = &pin
	if _, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("visionos", "visionos"), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil); err != nil {
		t.Fatalf("DownloadManifestlessDash: %v", err)
	}
	if !hasLogPrefix(logs, "[FormatSelector] Manual video itag 303 not offered") {
		t.Error("an unhonoured video pin was not warned")
	}

	job, logs = vodPotJob(t)
	apin := 251
	job.Job.SelectedAudioItag = &apin
	if _, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("visionos", "visionos"), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil); err != nil {
		t.Fatalf("DownloadManifestlessDash: %v", err)
	}
	if !hasLogPrefix(logs, "[FormatSelector] Manual audio itag 251 not offered") {
		t.Error("an unhonoured audio pin was not warned")
	}

	job, logs = vodPotJob(t)
	honoured := 299
	job.Job.SelectedVideoItag = &honoured
	if _, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("visionos", "visionos"), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil); err != nil {
		t.Fatalf("DownloadManifestlessDash: %v", err)
	}
	if hasLogPrefix(logs, "[FormatSelector] Manual") {
		t.Error("an honoured pin was warned")
	}

	srv := newManifestServer(t, potTestMaster)
	job, logs = vodPotJob(t)
	job.Job.SelectedVideoItag = &honoured
	if _, err := DownloadHls(context.Background(), job, &youtube.VideoInfo{
		StreamStatus: youtube.StreamLive, HlsManifestURL: srv.URL + "/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "visionos",
	}, nil, nil, &bgutils.PotProvider{}, nil); err != nil {
		t.Fatalf("DownloadHls: %v", err)
	}
	if !hasLogPrefix(logs, "[FormatSelector] Manual video itag 299 cannot be honoured on an HLS stream") {
		t.Error("a pin on an HLS stream was ignored silently")
	}
}

package worker

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// partBaseRe extracts the shared base from a part filename
// ("{base} - partN.mp4"). muxSegment uses it to pin every later part to the
// FIRST recorded part's base name — see the pinning comment there.
var partBaseRe = regexp.MustCompile(`^(.+) - part\d+\.mp4$`)

// writeDescriptionAtomic writes the description via tmp+rename so a crash
// mid-write can't leave a partially-written .description file that the DB
// row still points at. The tmp file is cleaned up on rename failure.
func writeDescriptionAtomic(finalPath, body string) error {
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// resolveFreshFilename resolves the filename template against fresh job
// metadata (title, channel name, and start time may have been updated during
// stream processing). Returns the resolved name (falling back to the current
// jobCtx.Filename when the lookup or template comes up empty) and the fresh
// job row (nil when the lookup failed). Does NOT mutate jobCtx — background
// part-mux goroutines call this concurrently with the download loop.
func (o *DownloadOrchestrator) resolveFreshFilename(jobCtx *JobContext) (string, *database.Job) {
	freshJob, err := o.db.GetJob(jobCtx.Job.ID)
	if err != nil || freshJob == nil {
		return jobCtx.Filename, nil
	}
	template := jobCtx.Config.FilenameTemplate
	var dateStr *string
	if freshJob.StreamStartTime != "" {
		dateStr = &freshJob.StreamStartTime
	} else if freshJob.CreatedAt != "" {
		dateStr = &freshJob.CreatedAt
	}
	templateID := freshJob.VideoID
	if freshJob.Platform == "twitch" {
		templateID = freshJob.ID
	}
	resolved := config.ResolveTemplate(template, config.TemplateVariables{
		Title:   freshJob.Title,
		ID:      templateID,
		Channel: freshJob.ChannelName,
		Date:    dateStr,
	})
	if resolved == "" {
		resolved = jobCtx.Filename
	}
	return resolved, freshJob
}

const (
	// muxShortfallTolerance is how much shorter than its input a muxed output
	// may be before it is treated as truncated. Two Twitch VOD segments'
	// worth (10 s each) — the coarsest segment duration in play — so ordinary
	// container-metadata rounding never trips it.
	muxShortfallTolerance = 20 * time.Second
	// muxDurationCheckFloor is the shortest input the check judges at all. A
	// fragmented raw stream's container metadata is unreliable and often
	// reports a fraction of the real length, so only a plausibly long input
	// is compared; anything shorter is skipped rather than guessed at.
	muxDurationCheckFloor = 60 * time.Second
)

// muxedOutputIsShort reports whether a muxed output is short enough to mean
// FFmpeg's `-c copy` stopped at the first undemuxable fragment. It exits 0
// when that happens, so a truncated .mp4 used to finish as a clean job with
// nothing comparing it against what went in (sweep-2 ENGINE-9).
//
// Both durations are seconds as ffprobe reports them. The check is
// deliberately one-sided and conservative: an input the probe could not read
// (0) or one too short to judge is never flagged, and an output LONGER than
// its input never is either.
func muxedOutputIsShort(inputSec, outputSec float64) bool {
	if inputSec < muxDurationCheckFloor.Seconds() || outputSec <= 0 {
		return false
	}
	return inputSec-outputSec > muxShortfallTolerance.Seconds()
}

// verifyMuxedDuration probes the longest input beside the already-probed
// output and returns an error naming both durations when the output is short.
//
// Failing the mux is the point: the alternative this replaces was a Finished
// row over a third of a recording, with the staging the missing part still
// lives in deleted on the way out. An error leaves the job in Error with the
// numbers in its message and staging untouched (both cleanup paths only run
// after a mux that returned nil), so the Mux action can re-run the copy once
// the input is repaired. Both call sites discard the rejected output first
// (discardShortMuxOutput) — the footage is in the INPUT, and leaving a short
// file under the archive's name is what the re-mux has to write over.
//
// The flag path is deliberately NOT used here: incomplete_tail means "the
// DOWNLOAD is missing segments" and steers Retry into re-downloading, which
// fixes nothing when the bytes are already on disk and it is the copy that
// stopped early.
func (o *DownloadOrchestrator) verifyMuxedDuration(ctx context.Context, jobID string, outputSec float64, inputs ...string) error {
	var longest float64
	for _, in := range inputs {
		if in == "" {
			continue
		}
		if probe := o.runFFprobe(ctx, in); probe != nil && probe.DurationSec > longest {
			longest = probe.DurationSec
		}
	}
	if !muxedOutputIsShort(longest, outputSec) {
		return nil
	}
	o.logger.Error("muxed output is shorter than its input — the copy stopped at a bad fragment",
		"jobID", jobID, "inputSeconds", longest, "outputSeconds", outputSec)
	return fmt.Errorf("mux produced %.0fs from a %.0fs input (%.0fs missing) — the copy stopped at a bad fragment; the short output was removed and staging is preserved, re-run the Mux action",
		outputSec, longest, longest-outputSec)
}

// discardShortMuxOutput deletes the output a shortfall verdict just rejected.
//
// The alternative is what shipped with ENGINE-9: the truncated .mp4 stayed in
// the output directory wearing the archive's own name while the row went to
// Error with output_file empty, so no UI could play or delete it, the orphan
// sweep offered it as an unowned file, and the Mux action's retry landed
// beside a bad file rather than over it. The footage is not lost by this —
// the INPUT is what holds it, and staging is preserved for exactly that
// reason. Best-effort: a removal that fails leaves the old situation, which
// the error message already describes.
func (o *DownloadOrchestrator) discardShortMuxOutput(jobID, outputFile string) {
	if err := os.Remove(outputFile); err != nil && !os.IsNotExist(err) {
		o.logger.Warn("could not remove the short muxed output; it stays under the archive's name",
			"output", outputFile, "err", err, "jobID", jobID)
	}
}

// stagedRecordingParts returns the recordings inside ONE staging dir in
// RECORDING ORDER: first every <file>.restart-<ts> the engine set aside when
// a restart could not resume (oldest timestamp first — that is the order they
// were captured in), then the dir's live recording.
//
// The engine preserves rather than truncates a headed recording it cannot
// resume (engine.StagedRestartSuffix), and until this list existed nothing in
// the worker could see those files: they are not what discoverStagingMedia
// recognises, so they were invisible to the mux, to the part scan, and to
// every cleanup — which meant a fresh capture finishing cleanly deleted them.
// The sidecar twin (<file>.restart-<ts>.resume.json) is excluded by
// engine.IsStagedRestartPath: muxing a JSON file is not a recovery.
func stagedRecordingParts(dir string) []string {
	parts := stagedRestartAsides(dir)
	if media := discoverStagingMedia(dir); media != nil {
		live := media.VideoPath
		if live == "" {
			live = media.AudioPath
		}
		if live != "" {
			parts = append(parts, live)
		}
	}
	return parts
}

// stagedRestartAsides returns the set-aside recordings in ONE dir, oldest
// stamp first — stagedRecordingParts without the live recording, and so
// without discoverStagingMedia's Stat of every candidate media name.
//
// Split out because the orphan sweep asks for asides and nothing else, for
// every job, on every pass (jobNeedsStaging and scanStagingOrphans in
// internal/worker/orphans.go, via stagedAsideRecordings): one ReadDir per dir
// is the whole cost, and a result that never contains the live recording is
// also the correct ANSWER there — an ordinary staging file is not
// captured-and-never-muxed footage.
func stagedRestartAsides(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var asides []string
	for _, e := range entries {
		if e.IsDir() || !engine.IsStagedRestartPath(e.Name()) {
			continue
		}
		asides = append(asides, filepath.Join(dir, e.Name()))
	}
	sort.SliceStable(asides, func(i, j int) bool {
		return stagedRestartStamp(asides[i]) < stagedRestartStamp(asides[j])
	})
	return asides
}

// stagedRestartStamp is the unix timestamp the engine stamped into an aside's
// name, or 0 for anything else (which sorts such a name first — a name
// engine.IsStagedRestartPath already vouched for cannot reach that branch).
func stagedRestartStamp(path string) int64 {
	name := filepath.Base(path)
	i := strings.LastIndex(name, engine.StagedRestartSuffix)
	if i < 0 {
		return 0
	}
	stamp, err := strconv.ParseInt(name[i+len(engine.StagedRestartSuffix):], 10, 64)
	if err != nil {
		return 0
	}
	return stamp
}

// stagedAsideRecordings returns every set-aside recording under a job's
// staging tree — the root and each seg_N part dir — in recording order.
// Empty for the overwhelmingly common case of a staging dir the engine never
// had to set anything aside in.
func stagedAsideRecordings(stagingDir string) []string {
	var out []string
	dirs := []string{stagingDir}
	for _, sd := range stagedSegDirs(stagingDir) {
		dirs = append(dirs, sd.dir)
	}
	for _, dir := range dirs {
		out = append(out, stagedRestartAsides(dir)...)
	}
	return out
}

// asideGroup is one restart's worth of set-aside recordings: the video and
// audio halves the engine set aside together (a DASH capture runs one
// SegmentDownloader per stream and both stamp the same second), or whichever
// single file a single-stream capture left behind.
type asideGroup struct {
	stamp string   // the literal timestamp text, reused in the output name
	video string   // "" when the restart set aside audio only
	audio string   // "" for HLS/VOD captures and video-only DASH
	files []string // every file in the group, in the order the scan found them
}

// groupStagedAsides folds a flat list of asides into one group per (staging
// dir, timestamp), classifying each file by the stem the engine stamped —
// the same names discoverStagingMedia recognises. Input order is preserved,
// so groups come back oldest recording first.
func groupStagedAsides(asides []string) []asideGroup {
	var order []string
	byKey := map[string]*asideGroup{}
	for _, p := range asides {
		base := filepath.Base(p)
		i := strings.LastIndex(base, engine.StagedRestartSuffix)
		if i < 0 {
			continue // engine.IsStagedRestartPath already vouched for the name
		}
		stem, stamp := base[:i], base[i+len(engine.StagedRestartSuffix):]
		key := filepath.Dir(p) + "\x00" + stamp
		g := byKey[key]
		if g == nil {
			g = &asideGroup{stamp: stamp}
			byKey[key] = g
			order = append(order, key)
		}
		g.files = append(g.files, p)
		if stem == "audio_stream" || stem == "audio.m4a" {
			g.audio = p
		} else {
			g.video = p
		}
	}
	out := make([]asideGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// Aside is one restart's worth of set-aside recording, as the UIs see it: the
// GROUP, never the individual files. A DASH restart sets the video and audio
// halves aside under the same second and muxStagedAsides recombines them into
// a single output, so "two asides" must mean two recordings an operator can
// get back, not four files on disk.
type Aside struct {
	// Path is the group's video half (its audio half for an audio-only
	// capture) — enough to identify the recording in a tooltip or a log line,
	// and never used to address it: recovery works from the staging dir.
	Path string `json:"path"`
	// Size is every byte of the group, both halves included.
	Size int64 `json:"size"`
	// Timestamp is the second the engine stamped into the name, rendered as
	// RFC 3339 UTC — the project's wire format for a time. Empty only if the
	// stamp is unparseable, which engine.IsStagedRestartPath already excludes.
	Timestamp string `json:"timestamp"`
	// HasResumeSidecar reports whether the aside still has its .resume.json
	// twin. Informational: recovery muxes the bytes either way, and the twin
	// is deleted with the recording it describes.
	HasResumeSidecar bool `json:"hasResumeSidecar"`
}

// AsideReport is one job's staging directory as the recovery surfaces read it.
type AsideReport struct {
	// Groups is in stagedAsideRecordings' order: the staging root's asides
	// first, then each seg_N part dir's, oldest first WITHIN each — a split
	// job's part-1 restart therefore follows a later root restart. Never nil:
	// the UIs index into it, and a job that never staged anything must answer
	// [] rather than null.
	Groups []Aside `json:"groups"`
	// KeptChatSidecar reports whether the staging dir still holds the chat
	// capture keepOnlyChatCapture preserves (worker.go). A recovery carries it
	// beside the first recovered file, which is the only way it ever reaches
	// the output directory for a job whose own finalize had no chat to copy.
	//
	// Only computed when Groups is non-empty: finding it costs a tree walk,
	// this report is built on every GET /api/jobs/{id}, and neither UI renders
	// the flag without a recording to recover beside.
	KeptChatSidecar bool `json:"keptChatSidecar"`
}

// ScanAsides reports the set-aside recordings in one job's staging directory.
//
// Takes (stagingBase, jobID) rather than a directory for the same reason
// HasSegmentFiles (staging.go) does: the REST layer already reads the base out
// of the config store and has no worker instance to ask.
func ScanAsides(stagingBase, jobID string) AsideReport {
	return asideReport(filepath.Join(stagingBase, jobID))
}

// asideReport is ScanAsides over an already-resolved directory — the form the
// orchestrator uses, where the job context carries the path.
func asideReport(stagingDir string) AsideReport {
	groups := groupStagedAsides(stagedAsideRecordings(stagingDir))
	out := AsideReport{Groups: make([]Aside, 0, len(groups))}
	if len(groups) == 0 {
		// Short-circuit the tree walk. This runs on every GET /api/jobs/{id},
		// and the overwhelmingly common answer is "nothing was ever set
		// aside" — for which the chat flag is not rendered by either UI
		// anyway, because both gate the whole section on having a recording
		// to recover.
		return out
	}
	out.KeptChatSidecar = findKeptChatCapture(stagingDir) != ""
	for _, g := range groups {
		// groupStagedAsides classifies every file into video or audio, and a
		// group only exists because a file landed in one of them, so exactly
		// one of these is set for a single-stream capture and video wins for a
		// DASH pair.
		path := g.video
		if path == "" {
			path = g.audio
		}
		a := Aside{Path: path, Timestamp: asideTimestamp(g.stamp)}
		for _, p := range g.files {
			if info, err := os.Stat(p); err == nil {
				a.Size += info.Size()
			}
			if fileExists(engine.StagedRestartSidecar(p)) {
				a.HasResumeSidecar = true
			}
		}
		out.Groups = append(out.Groups, a)
	}
	return out
}

// asideTimestamp renders the unix-seconds stamp the engine wrote into an
// aside's name as RFC 3339 UTC. Empty for anything unparseable — which
// engine.IsStagedRestartPath has already excluded by the time a name reaches
// here, so the empty return is a guard, not a case.
func asideTimestamp(stamp string) string {
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339)
}

// findKeptChatCapture returns the chat capture a preserved staging dir still
// holds, or "" when there is none.
//
// The set of files that COUNT as the chat capture is isChatCaptureFile's
// (worker.go) — the same rule keepOnlyChatCapture prunes down to, so the two
// cannot disagree about what survived. The file this returns is the narrow
// member of that set: chat.json itself is the only one a recovery can copy
// beside an output, because chat.json.resume.json is a resume offset map and
// chat.json.lostbatch.json is a spill, and neither is a chat archive.
//
// Any depth, because a quality- or gap-split job keeps each part's chat beside
// that part's media in seg_N/.
func findKeptChatCapture(stagingDir string) string {
	if stagingDir == "" {
		return ""
	}
	var found string
	_ = filepath.WalkDir(stagingDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !isChatCaptureFile(name) {
			return nil
		}
		if name == "chat.json" {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// asideOutputCollisionLimit bounds asideOutputPath's counter. Reaching it
// needs a hundred asides sharing one stem and one second, which no capture
// produces; the bound exists so the search can never run away, and the false
// return so it can never hand back a name it did not check.
const asideOutputCollisionLimit = 100

// asideOutputPath is where one group's recovered file lands: the archive's own
// name with the aside's suffix on it, so the two sort together in the output
// directory. Two groups can only collide when a root and a seg_N restart share
// a second; the counter keeps both files rather than overwriting one. Reports
// false when every name within the bound is taken — the caller must then leave
// the aside in staging rather than write over somebody's archive.
func asideOutputPath(outputDir, filenameBase, stamp string, used map[string]bool) (string, bool) {
	base := filepath.Join(outputDir, filenameBase+engine.StagedRestartSuffix+stamp)
	for n := 1; n <= asideOutputCollisionLimit; n++ {
		candidate := base + ".mp4"
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d.mp4", base, n)
		}
		if used[normalizePath(candidate)] || fileExists(candidate) {
			continue
		}
		used[normalizePath(candidate)] = true
		return candidate, true
	}
	return "", false
}

// muxStagedAsides muxes every recording the engine set aside into its own file
// beside the job's archive, then deletes it and its resume twin.
//
// An aside is a headed recording the no-truncate guard could not resume
// (engine.StagedRestartSuffix). The fresh capture that replaced it restarted
// at sq=0, so the two OVERLAP: concatenating them into the archive would
// replay the opening, and registering the recovered file as a part would put
// that replay in the job's part list. It is therefore a SIBLING — no segment
// row, surfaced by name in the Warn below, which is the only place an operator
// learns it exists.
//
// Best-effort by design. A mux that fails leaves the aside exactly where it
// was, where hasUnmuxedPartsForJob keeps the whole staging dir from being
// swept — and a later Mux action retries it WHILE THE DIR STILL HOLDS
// RECOGNISED MEDIA; an aside-only dir is not offered the Mux action at all,
// because HasSegmentFiles (discoverStagingMedia) does not know the suffix.
// Either way it never fails the job, whose own recording muxed fine.
func (o *DownloadOrchestrator) muxStagedAsides(ctx context.Context, jobCtx *JobContext, outputDir, filenameBase string) {
	groups := groupStagedAsides(stagedAsideRecordings(jobCtx.StagingDir))
	if len(groups) == 0 {
		return
	}
	used := map[string]bool{}
	for _, g := range groups {
		out, ok := asideOutputPath(outputDir, filenameBase, g.stamp, used)
		if !ok {
			o.logger.Error("no free name for a recovered set-aside recording; it stays in staging",
				"aside", strings.Join(g.files, " | "), "tried", filenameBase+engine.StagedRestartSuffix+g.stamp,
				"limit", asideOutputCollisionLimit, "jobID", jobCtx.Job.ID)
			continue
		}
		if err := o.mux().MuxCopy(ctx, g.video, g.audio, out); err != nil {
			// Take the partial with it. engine.cleanupFailedMux deliberately
			// PRESERVES a partial output when the failure was a ctx cancel —
			// right for the main mux, where the partial is all the operator
			// has — but an aside's partial has no salvage value: the aside
			// itself is still in staging and the next finalize re-muxes it.
			// Left behind, it is a moov-less file wearing the archive's own
			// stem, which the output sweep OWNS (engine.RestartSiblingStem)
			// and so never offers for deletion, while asideOutputPath's
			// counter writes the successful retry to "-2" beside it.
			os.Remove(out)
			o.logger.Error("could not mux a set-aside recording; it stays in staging and the dir is kept for a later Mux action",
				"aside", strings.Join(g.files, " | "), "err", err, "jobID", jobCtx.Job.ID)
			continue
		}
		o.logger.Warn("a set-aside recording was muxed to its own file beside the archive; it overlaps the start of the main recording, so it is NOT one of the job's parts",
			"output", out, "aside", strings.Join(g.files, " | "), "jobID", jobCtx.Job.ID)
		for _, p := range g.files {
			if err := os.Remove(p); err != nil {
				o.logger.Warn("could not remove a recovered set-aside recording", "aside", p, "err", err, "jobID", jobCtx.Job.ID)
			}
			if err := os.Remove(engine.StagedRestartSidecar(p)); err != nil && !os.IsNotExist(err) {
				o.logger.Warn("could not remove a recovered aside's resume sidecar", "sidecar", engine.StagedRestartSidecar(p), "err", err, "jobID", jobCtx.Job.ID)
			}
		}
	}
}

func (o *DownloadOrchestrator) muxAndFinalize(ctx context.Context, jobCtx *JobContext, result *DownloadResult) error {
	o.logger.Info("muxing", "jobID", jobCtx.Job.ID)

	// Re-resolve filename template with fresh metadata (matches TS muxFinalize behavior).
	if resolved, freshJob := o.resolveFreshFilename(jobCtx); freshJob != nil {
		jobCtx.Filename = resolved
		// Update local job reference for notifications below
		jobCtx.Job = freshJob
	}

	// Recover parts whose mux never persisted a segment row — a daemon
	// restart killed the background FFmpeg before AddSegment, or an
	// in-process part mux failed and was only logged. This must run BEFORE
	// the finalize shape is decided: without it the multi-segment path sees
	// only the recorded rows, the single-part rename can promote a LATER
	// part to the plain name as if it were the whole recording, and
	// processJob's staging cleanup then deletes the unmuxed media for good.
	// No-op when the job has no seg_N staging dirs.
	o.muxUnrecordedSegments(ctx, jobCtx)

	// Multi-segment path: if the job has segments (from part splitting),
	// the individual part .mp4 files are already muxed. We just need to
	// handle assets (chat, thumbnail, description) and set the job as finished.
	if segments, err := o.db.GetSegments(jobCtx.Job.ID); err != nil {
		o.logger.Warn("failed to check segments, falling back to single-file mux", "err", err, "jobID", jobCtx.Job.ID)
	} else if len(segments) > 0 {
		return o.finalizeMultiSegmentJob(ctx, jobCtx, segments)
	}

	freshJob := o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})
	if freshJob == nil {
		freshJob = jobCtx.Job
	}

	// Send "Muxing Starting" notification with enriched fields (matching TypeScript muxFinalize)
	if o.notifier != nil {
		var muxFields []notifications.Field
		if freshJob.LastVideoSeq != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Video Segments", Value: fmt.Sprintf("%d", *freshJob.LastVideoSeq), Inline: true})
		}
		if freshJob.LastAudioSeq != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Audio Segments", Value: fmt.Sprintf("%d", *freshJob.LastAudioSeq), Inline: true})
		}
		if freshJob.TotalChatMessages != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Chat Messages", Value: fmt.Sprintf("%d", *freshJob.TotalChatMessages), Inline: true})
		}
		if freshJob.DownloadStartedAt != "" {
			if startTime, err := time.Parse(time.RFC3339, freshJob.DownloadStartedAt); err == nil {
				elapsed := time.Since(startTime)
				muxFields = append(muxFields, notifications.Field{Name: "Download Time", Value: formatDurationHuman(elapsed), Inline: true})
			}
		}
		o.notifier.Send("Muxing Starting",
			fmt.Sprintf("Download complete, muxing: %s", jobCtx.Job.Title),
			notifications.TypeMuxing,
			muxFields,
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Thumbnail: jobCtx.Job.ThumbnailURL,
				Event:     "muxing",
			},
		)
	}

	// Resolve output path — template may contain subdirectory (e.g. "${channel}/...")
	filenameBase := jobCtx.Filename
	outputFile := filepath.Join(jobCtx.OutputDir, filenameBase+".mp4")
	outputDir := filepath.Dir(outputFile)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	// relBase preserves the full relative path (including channel subdir) for DB
	// storage, so the web handler can resolve files via Join(outputDir, filename).
	relBase := filenameBase
	// Strip subdirectory from filenameBase so asset writes (chat, description,
	// thumbnail) use just the filename, not the full template path. outputDir
	// already includes any subdirectory from the template.
	filenameBase = filepath.Base(filenameBase)

	// Recover anything the no-truncate guard set aside into its own sibling
	// file. Before the main mux on purpose: the aside is footage this job
	// captured, and a main mux that fails (ENGINE-9's short-output check, a
	// missing FFmpeg) must not be what decides whether it is ever readable.
	// The multi-segment shape reaches its own call in finalizeMultiSegmentJob,
	// which this function has already returned into by here — exactly one call
	// per finalize.
	o.muxStagedAsides(ctx, jobCtx, outputDir, filenameBase)

	videoPath := result.VideoPath
	audioPath := result.AudioPath

	// Only mux if we have files
	if videoPath != "" {
		if _, err := os.Stat(videoPath); err != nil {
			videoPath = ""
		}
	}
	if audioPath != "" {
		if _, err := os.Stat(audioPath); err != nil {
			audioPath = ""
		}
	}

	if videoPath == "" && audioPath == "" {
		return fmt.Errorf("no media files to mux")
	}

	if err := o.mux().MuxCopy(ctx, videoPath, audioPath, outputFile); err != nil {
		return fmt.Errorf("mux: %w", err)
	}

	// B5: Run ffprobe to extract actual video metadata
	probeData := o.runFFprobe(ctx, outputFile)

	// ENGINE-9: before anything writes Finished over this row, check that the
	// copy actually carried the recording across. A short output errors out
	// here with staging intact rather than finishing clean over a fraction of
	// the archive.
	if probeData != nil {
		if err := o.verifyMuxedDuration(ctx, jobCtx.Job.ID, probeData.DurationSec, videoPath, audioPath); err != nil {
			o.discardShortMuxOutput(jobCtx.Job.ID, outputFile)
			return err
		}
	}

	// Get file info
	info, err := os.Stat(outputFile)
	if err != nil {
		o.logger.Warn("stat output file", "err", err)
	}

	// Update job status (clear progress fields like TS muxFinalize)
	updates := map[string]any{
		"status":      database.StatusFinished,
		"output_file": outputFile,
		"filename":    relBase + ".mp4",
		"progress":    "",
		"percent":     100.0,
		"speed":       "",
		"eta":         "",
	}
	if info != nil {
		updates["file_size"] = info.Size()
	}

	// Prefer ffprobe metadata over format metadata
	if probeData != nil {
		if probeData.Width > 0 {
			updates["video_width"] = probeData.Width
		}
		if probeData.Height > 0 {
			updates["video_height"] = probeData.Height
		}
		if probeData.Fps > 0 {
			updates["video_fps"] = probeData.Fps
		}
		if probeData.DurationSec > 0 {
			updates["length_seconds"] = int(probeData.DurationSec)
		}
	} else if result.VideoFormat != nil {
		// Fallback to format metadata
		if result.VideoFormat.Width != nil {
			updates["video_width"] = *result.VideoFormat.Width
		}
		if result.VideoFormat.Height != nil {
			updates["video_height"] = *result.VideoFormat.Height
		}
		if result.VideoFormat.Fps != nil {
			updates["video_fps"] = *result.VideoFormat.Fps
		}
	}

	// Copy assets (chat, description, thumbnail) to output directory
	o.copyAssets(ctx, jobCtx, outputDir, filenameBase, relBase, updates, false)

	o.keepIncompleteTailProgress(jobCtx.Job.ID, updates)
	finishedJob := o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send "Download Finished" notification
	o.sendFinishedNotification(jobCtx, finishedJob, outputFile, probeData, info)

	o.logger.Info("download complete", "jobID", jobCtx.Job.ID, "output", outputFile)
	return nil
}

// keepIncompleteTailProgress strips the progress/percent overwrites from a
// finalize update map when the job's recording is known to be missing tail
// segments, so the honest values written when the download gave up survive
// into the Finished row.
//
// Both finalize paths otherwise hard-code percent=100 (and blank the progress
// string) for every job that muxes successfully — which is right for a clean
// capture but would put a full progress bar under a knowingly-truncated one
// (the Web UI renders percent unconditionally, so the bar would contradict
// the "Incomplete tail" badge sitting beside it). Deleting the keys rather
// than recomputing them keeps the single source of truth in the orchestrator
// where the seq/head numbers actually live.
//
// The flag is re-read rather than threaded through: it is written after the
// download loop returns but before finalize runs, so the in-memory
// jobCtx.Job copy predates it. A read failure leaves the map untouched —
// i.e. falls back to the pre-existing 100% behavior.
func (o *DownloadOrchestrator) keepIncompleteTailProgress(jobID string, updates map[string]any) {
	fresh, err := o.db.GetJob(jobID)
	if err != nil || fresh == nil || !fresh.IncompleteTail {
		return
	}
	delete(updates, "progress")
	delete(updates, "percent")
}

// finalizeMultiSegmentJob handles the finalization path for jobs with quality-split segments.
// Individual segment .mp4 files are already muxed; this method copies assets and updates the job.
func (o *DownloadOrchestrator) finalizeMultiSegmentJob(ctx context.Context, jobCtx *JobContext, segments []database.Segment) error {
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})

	// Tier 4: opportunistically collapse contiguous same-format parts into
	// one file BEFORE anything below decides the finalize shape, so a
	// fully-merged job takes the plain output name (the len==1 check just
	// below) exactly like a never-split job. Runs under the Muxing status
	// set just above rather than the stale pre-finalize status, since a
	// multi-run concat can take a while.
	//
	// YouTube-only gate. Twitch gap-split parts are NOT eligible, for three
	// reasons: (1) when chat is off, Twitch's gap-split parts are
	// deliberately gapless-part semantics -- each part is a distinct
	// continuous span with a real gap between them, and collapsing them
	// back into one file destroys that meaning; (2) when every part's chat
	// happens to be empty, mergeChatFiles has nothing to schema-mismatch
	// on, so the merge "succeeds" and deletes the per-part
	// twitch.TwitchChatData files, replacing them with a YouTube-shaped
	// chat.ChatData husk that doesn't match Twitch's chat schema; (3) when
	// chat IS present and non-empty, the run still pays the full video
	// concat-copy (I/O, disk, time) before mergeChatFiles's schema
	// mismatch (TwitchChatData.message is a string, chat.ChatData.message
	// is []MessagePart) aborts the run -- a throwaway concat on every
	// chat-on Twitch multi-part finalize. Gating here at the call site
	// avoids all three: Twitch simply never attempts a Tier 4 merge.
	if jobCtx.Job.Platform == "youtube" {
		segments = o.mergeSameFormatParts(ctx, jobCtx, segments)
	}

	filenameBase := jobCtx.Filename
	outputDir := filepath.Join(jobCtx.OutputDir, filepath.Dir(filenameBase))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	filenameBase = filepath.Base(filenameBase)
	relBase := jobCtx.Filename

	// The multi-segment half of the aside recovery (see muxStagedAsides).
	// muxAndFinalize returns into this function before its own call, so a job
	// muxes its asides exactly once whichever shape it finalizes in.
	o.muxStagedAsides(ctx, jobCtx, outputDir, filenameBase)

	// A job that ends with exactly one part (e.g. an outage muxed part 1 and
	// the stream never came back) shouldn't keep a " - part1" suffix — single
	// outputs use the plain template name, exactly like jobs that never split.
	if len(segments) == 1 {
		segments[0] = o.renameSinglePartToPlain(segments[0], outputDir, filenameBase)
	}

	// Calculate total file size and duration from segments
	var totalSize int64
	var totalDuration float64
	for _, seg := range segments {
		if seg.FileSize != nil {
			totalSize += *seg.FileSize
		}
		totalDuration += seg.DurationSeconds
	}

	// Use first segment's resolution for the job metadata
	updates := map[string]any{
		"status":   database.StatusFinished,
		"filename": relBase,
		"progress": "",
		"percent":  100.0,
		"speed":    "",
		"eta":      "",
	}

	if totalSize > 0 {
		updates["file_size"] = totalSize
	}
	if totalDuration > 0 {
		updates["length_seconds"] = int(totalDuration)
	}

	// Use the first segment's quality for job-level metadata
	if len(segments) > 0 {
		if segments[0].VideoWidth != nil {
			updates["video_width"] = *segments[0].VideoWidth
		}
		if segments[0].VideoHeight != nil {
			updates["video_height"] = *segments[0].VideoHeight
		}
		if segments[0].VideoFps != nil {
			updates["video_fps"] = *segments[0].VideoFps
		}
		// Set output_file to the first segment (for backward compat with video route)
		updates["output_file"] = segments[0].FilePath
	}

	// Per-part chat: when parts carry their own chat files, the staging
	// chat.json belongs to part 1 (already enriched + copied at its mux) —
	// the legacy whole-job copy in copyAssets would duplicate it under the
	// plain name. The job-level chat fields follow the FIRST part only, so
	// the dashboard player (which plays output_file = first part) replays
	// the matching chat; later parts' chat is reachable via segment rows.
	anyPartChat := false
	for _, seg := range segments {
		if seg.ChatFile != "" {
			anyPartChat = true
			break
		}
	}
	if anyPartChat && segments[0].ChatFile != "" {
		updates["chat_file"] = segments[0].ChatFile
		updates["chat_filename"] = filepath.Join(filepath.Dir(relBase), filepath.Base(segments[0].ChatFile))
		updates["chat_status"] = chatFileStatus(jobCtx)
	}

	// Copy assets
	o.copyAssets(ctx, jobCtx, outputDir, filenameBase, relBase, updates, anyPartChat)

	o.keepIncompleteTailProgress(jobCtx.Job.ID, updates)
	o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send notification
	if o.notifier != nil {
		finishedJob, _ := o.db.GetJob(jobCtx.Job.ID)
		if finishedJob == nil {
			finishedJob = jobCtx.Job
		}

		var qualityLabels []string
		for _, seg := range segments {
			qualityLabels = append(qualityLabels, seg.Quality)
		}
		finFields := []notifications.Field{
			{Name: "Segments", Value: fmt.Sprintf("%d segments", len(segments)), Inline: true},
			{Name: "Qualities", Value: strings.Join(qualityLabels, " -> "), Inline: true},
			{Name: "Total Size", Value: formatFileSize(totalSize), Inline: true},
			{Name: "Duration", Value: formatDurationHuman(time.Duration(totalDuration) * time.Second), Inline: true},
		}
		// Resolution from first segment
		if len(segments) > 0 && segments[0].VideoWidth != nil && segments[0].VideoHeight != nil &&
			*segments[0].VideoWidth > 0 && *segments[0].VideoHeight > 0 {
			res := fmt.Sprintf("%dx%d", *segments[0].VideoWidth, *segments[0].VideoHeight)
			if segments[0].VideoFps != nil && *segments[0].VideoFps > 0 {
				res += fmt.Sprintf(" @%dfps", *segments[0].VideoFps)
			}
			finFields = append(finFields, notifications.Field{Name: "Resolution", Value: res, Inline: true})
		}
		// Total time
		if finishedJob.DownloadStartedAt != "" {
			if startedAt, err := time.Parse(time.RFC3339, finishedJob.DownloadStartedAt); err == nil {
				finFields = append(finFields, notifications.Field{
					Name: "Total Time", Value: formatDurationHuman(time.Since(startedAt)), Inline: true,
				})
			}
		}
		// Chat messages
		if finishedJob.TotalChatMessages != nil && *finishedJob.TotalChatMessages > 0 {
			finFields = append(finFields, notifications.Field{
				Name: "Chat Messages", Value: fmt.Sprintf("%d", *finishedJob.TotalChatMessages), Inline: true,
			})
		}
		o.notifier.Send("Download Finished",
			fmt.Sprintf("Successfully archived: %s", jobCtx.Job.Title),
			notifications.TypeSuccess,
			finFields,
			notifications.SendOptions{
				URL:   jobCtx.Job.URL,
				Image: jobCtx.Job.ThumbnailURL,
				Event: "finished",
			},
		)
	}

	o.logger.Info("multi-segment download complete",
		"jobID", jobCtx.Job.ID, "segments", len(segments))
	return nil
}

// renameSinglePartToPlain drops the " - part1" suffix from a job's only part
// — the file, its chat sibling, and the segment row all move to the plain
// template name so a one-part outcome looks identical to a job that never
// split. Best-effort: on any rename failure the part keeps its suffixed name
// and the row is returned unchanged.
//
// outputDir/filenameBase are the FRESH template-resolved values, so on a
// mid-job retitle/rechannel this can move the single part into a different
// directory than where muxSegment originally wrote it (under the first part's
// pinned name). That relocation is intentional: it places the one-part output
// exactly where a never-split job's single output lands (muxAndFinalize uses
// the same fresh template dir), keeping the two outcomes indistinguishable.
func (o *DownloadOrchestrator) renameSinglePartToPlain(seg database.Segment, outputDir, filenameBase string) database.Segment {
	plainVideo := filepath.Join(outputDir, filenameBase+".mp4")
	if seg.FilePath == "" || seg.FilePath == plainVideo {
		return seg
	}
	if err := utils.ReplaceFile(seg.FilePath, plainVideo); err != nil {
		// Recovery re-entry: a crash between a prior run's successful rename
		// and its DB commit leaves the file already at plainVideo while the row
		// still points at the suffixed name. Here the rename fails (source gone)
		// but the destination exists and holds the real media — treat that as
		// success and fall through to repair the row, so output_file doesn't
		// resolve to the missing suffixed path (which the orphan scanner would
		// then offer for deletion). Any other failure keeps the suffixed name.
		if _, dstErr := os.Stat(plainVideo); os.IsNotExist(dstErr) {
			o.logger.Warn("failed to rename single part to plain name", "err", err, "from", seg.FilePath)
			return seg
		}
		o.logger.Info("single-part rename source already relocated; repairing segment row to plain name",
			"plain", plainVideo, "jobID", seg.JobID)
	}
	renamed := seg
	renamed.FilePath = plainVideo
	renamed.Filename = filenameBase + ".mp4"

	if seg.ChatFile != "" {
		plainChat := filepath.Join(outputDir, filenameBase+".chat.json")
		if err := utils.ReplaceFile(seg.ChatFile, plainChat); err != nil {
			// Same recovery re-entry as the video above: if the chat is already
			// at the plain path (a prior run renamed it pre-crash), adopt it.
			if _, dstErr := os.Stat(plainChat); dstErr == nil {
				renamed.ChatFile = plainChat
			} else {
				o.logger.Warn("failed to rename single part chat to plain name", "err", err, "from", seg.ChatFile)
			}
		} else {
			renamed.ChatFile = plainChat
		}
	}

	if err := o.db.UpdateSegmentFile(renamed.ID, renamed.Filename, renamed.FilePath, renamed.ChatFile); err != nil {
		o.logger.Warn("failed to update renamed segment row", "err", err, "segmentID", renamed.ID)
	}
	o.logger.Info("single-part job renamed to plain output name",
		"jobID", seg.JobID, "file", renamed.Filename)
	return renamed
}

// copyAssets copies chat, description, and thumbnail files to the output directory.
// Updates the provided map with file paths for DB storage. skipChat suppresses
// the whole-job chat copy for jobs whose parts carry per-part chat files (the
// staging chat.json is the last part's chat, already handled at part-mux time).
func (o *DownloadOrchestrator) copyAssets(ctx context.Context, jobCtx *JobContext, outputDir, filenameBase, relBase string, updates map[string]any, skipChat bool) {
	// Copy chat file to output directory
	chatSrc := filepath.Join(jobCtx.StagingDir, "chat.json")
	if _, err := os.Stat(chatSrc); err == nil && !skipChat {
		chatBaseName := filenameBase + ".chat.json"
		chatDst := filepath.Join(outputDir, chatBaseName)
		if err := copyFile(chatSrc, chatDst); err != nil {
			o.logger.Warn("failed to copy chat file", "err", err)
		} else {
			updates["chat_file"] = chatDst
			updates["chat_filename"] = relBase + ".chat.json"
			updates["chat_status"] = chatFileStatus(jobCtx)
		}
	}

	// Save video description as .description (matching TypeScript assetDownloader).
	// Atomic write via tmp+rename: a crash mid-write would otherwise leave a
	// truncated .description file with the DB row pointing at it.
	if jobCtx.Job.Description != "" {
		descPath := filepath.Join(outputDir, filenameBase+".description")
		if err := writeDescriptionAtomic(descPath, jobCtx.Job.Description); err != nil {
			o.logger.Warn("failed to save description", "err", err)
		} else {
			updates["description_file"] = descPath
		}
	}

	// Download thumbnail — check staging first (Twitch pre-downloads while live)
	thumbnailSaved := false
	for _, ext := range []string{".jpg", ".webp", ".png"} {
		stagingThumb := filepath.Join(jobCtx.StagingDir, "thumbnail"+ext)
		if _, err := os.Stat(stagingThumb); err == nil {
			thumbDst := filepath.Join(outputDir, filenameBase+ext)
			if err := copyFile(stagingThumb, thumbDst); err == nil {
				thumbnailSaved = true
				updates["thumbnail_file"] = thumbDst
			}
			break
		}
	}
	if !thumbnailSaved && jobCtx.Job.VideoID != "" {
		// YouTube thumbnail quality progression. Order matters because
		// only `maxresdefault` (1280x720) and `mqdefault` (320x180) are
		// natively 16:9 -- the others (sddefault 640x480, hqdefault
		// 480x360, default 120x90) are 4:3 with black bars baked into
		// the image. Once the Web UI prefers the local file, those 4:3
		// sources letterbox visibly inside the dashboard's 16:9 thumb
		// box. Try maxres first for quality, mqdefault second for clean
		// 16:9 framing, only then fall back to the letterboxed sizes.
		thumbQualities := []string{"maxresdefault", "mqdefault", "hqdefault", "sddefault", "default"}
		for _, quality := range thumbQualities {
			thumbURL := fmt.Sprintf("https://i.ytimg.com/vi/%s/%s.jpg", jobCtx.Job.VideoID, quality)
			thumbDst := filepath.Join(outputDir, filenameBase+".jpg")
			if DownloadFileMinSize(ctx, thumbURL, thumbDst, 1000, o.logger) == nil {
				thumbnailSaved = true
				updates["thumbnail_file"] = thumbDst
				break
			}
		}
	}
	if !thumbnailSaved {
		// Fallback: try explicit thumbnail URL or channel avatar
		thumbURL := jobCtx.Job.ThumbnailURL
		if thumbURL == "" {
			thumbURL = jobCtx.Job.ChannelAvatarURL
		}
		if thumbURL != "" {
			ext := ".jpg"
			if strings.Contains(thumbURL, ".webp") {
				ext = ".webp"
			}
			thumbDst := filepath.Join(outputDir, filenameBase+ext)
			if DownloadFileMinSize(ctx, thumbURL, thumbDst, 1000, o.logger) == nil {
				updates["thumbnail_file"] = thumbDst
			}
		}
	}
}

// sendFinishedNotification sends a "Download Finished" notification with enriched fields.
func (o *DownloadOrchestrator) sendFinishedNotification(jobCtx *JobContext, finishedJob *database.Job, outputFile string, probeData *ffprobeData, info os.FileInfo) {
	if o.notifier == nil {
		return
	}
	if finishedJob == nil {
		finishedJob = jobCtx.Job
	}
	fb := notifications.NewFieldBuilder()
	fb.Add("File", filepath.Base(outputFile))
	if probeData != nil && probeData.Width > 0 && probeData.Height > 0 {
		res := fmt.Sprintf("%dx%d", probeData.Width, probeData.Height)
		if probeData.Fps > 0 {
			res += fmt.Sprintf(" @%dfps", probeData.Fps)
		}
		fb.AddInline("Resolution", res)
	}
	if info != nil {
		fb.AddInline("File Size", formatFileSize(info.Size()))
	}
	if finishedJob.LengthSeconds != nil && *finishedJob.LengthSeconds > 0 {
		fb.AddInline("Duration", formatDurationHuman(time.Duration(*finishedJob.LengthSeconds)*time.Second))
	}
	if finishedJob.DownloadStartedAt != "" {
		if startTime, err := time.Parse(time.RFC3339, finishedJob.DownloadStartedAt); err == nil {
			fb.AddInline("Total Time", formatDurationHuman(time.Since(startTime)))
		}
	}
	if finishedJob.LastVideoSeq != nil {
		segStr := fmt.Sprintf("V: %d", *finishedJob.LastVideoSeq)
		if finishedJob.LastAudioSeq != nil {
			segStr += fmt.Sprintf(" A: %d", *finishedJob.LastAudioSeq)
		}
		fb.AddInline("Segments", segStr)
	}
	if finishedJob.TotalChatMessages != nil {
		fb.AddInline("Chat Messages", fmt.Sprintf("%d", *finishedJob.TotalChatMessages))
	}
	// Format selection (matching TS muxFinalize notification enrichment)
	if finishedJob.SelectedVideoItag != nil || finishedJob.SelectedAudioItag != nil {
		var formatInfo string
		if finishedJob.SelectedVideoItag != nil {
			if *finishedJob.SelectedVideoItag == -1 {
				formatInfo = "Video: None"
			} else {
				formatInfo = fmt.Sprintf("Video: itag %d", *finishedJob.SelectedVideoItag)
			}
		}
		if finishedJob.SelectedAudioItag != nil {
			if formatInfo != "" {
				formatInfo += ", "
			}
			if *finishedJob.SelectedAudioItag == -1 {
				formatInfo += "Audio: None"
			} else {
				formatInfo += fmt.Sprintf("Audio: itag %d", *finishedJob.SelectedAudioItag)
			}
		}
		fb.Add("Format Selection", formatInfo)
	}
	// Trimmed range
	if finishedJob.StartTime != nil || finishedJob.EndTime != nil {
		startStr := "0:00"
		endStr := "end"
		if finishedJob.StartTime != nil {
			startStr = FormatSecondsToTimestamp(*finishedJob.StartTime)
		}
		if finishedJob.EndTime != nil {
			endStr = FormatSecondsToTimestamp(*finishedJob.EndTime)
		}
		fb.AddInline("Trimmed Range", fmt.Sprintf("%s - %s", startStr, endStr))
	}
	// Description excerpt — Discord embeds cap field values around 1024
	// chars, but we keep the notification short. 297 + "..." == 300 total
	// (audit reports/worker.md F40).
	const descMaxLen = 300
	if finishedJob.Description != "" {
		desc := finishedJob.Description
		if len(desc) > descMaxLen {
			desc = desc[:descMaxLen-3] + "..."
		}
		fb.Add("Description", desc)
	}
	o.notifier.Send("Download Finished",
		fmt.Sprintf("Successfully archived: %s", jobCtx.Job.Title),
		notifications.TypeSuccess,
		fb.Build(),
		notifications.SendOptions{
			URL:   jobCtx.Job.URL,
			Image: jobCtx.Job.ThumbnailURL,
			Event: "finished",
		},
	)
}

// muxSegment muxes a single part and persists it to the database. Called at
// part boundaries (quality split, gap split) to finalize the current capture
// before starting a new one, and by recovery for parts that never muxed.
//
// unixStart == 0 is a sentinel meaning "the caller can't know the part's
// true start": a restart resumed into staged data that pre-dates the session,
// so the session-local segmentStartTime would mis-stamp hours of pre-restart
// footage as starting at daemon restart. The start is then derived from the
// muxed output's probed duration (unixEnd - duration, clamped to the job's
// download start) — the same approximation segment recovery uses.
func (o *DownloadOrchestrator) muxSegment(
	ctx context.Context,
	jobCtx *JobContext,
	segIdx int,
	unixStart, unixEnd int64,
	quality QualityInfo,
	result *DownloadResult,
) (*database.Segment, error) {
	// Part files carry the job's resolved name plus a 1-based part number —
	// "{name} - partN.mp4" — so they group and sort beside the job's other
	// assets. segIdx is stable across restarts and recovery (derived from
	// staging dir indices), which makes the name idempotent; short-skipped
	// segments can leave cosmetic holes in the numbering.
	filenameBase, _ := o.resolveFreshFilename(jobCtx)
	outputDir := filepath.Join(jobCtx.OutputDir, filepath.Dir(filenameBase))
	// Pin the base — and the directory — to the first recorded part: the
	// template resolves against live metadata, and a mid-job retitle or
	// channel rename (a restart re-processes stream info) would otherwise
	// scatter one recording's parts across different names or folders. The
	// directory comes from the first part's actual location rather than
	// re-resolving the template, so all parts of one recording stay
	// siblings. (finalizeMultiSegmentJob's single-part rename still
	// relocates a one-part outcome to the fresh template dir — see the
	// relocation note on renameSinglePartToPlain.)
	if segs, err := o.db.GetSegments(jobCtx.Job.ID); err == nil && len(segs) > 0 {
		if m := partBaseRe.FindStringSubmatch(segs[0].Filename); m != nil {
			filenameBase = filepath.Join(filepath.Dir(filenameBase), m[1])
			if segs[0].FilePath != "" {
				outputDir = filepath.Dir(segs[0].FilePath)
			}
		}
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create segment output dir: %w", err)
	}
	partBase := fmt.Sprintf("%s - part%d", filepath.Base(filenameBase), segIdx+1)
	segFilename := partBase + ".mp4"

	outputPath := filepath.Join(outputDir, segFilename)

	videoPath := result.VideoPath
	audioPath := result.AudioPath

	// Verify files exist
	if videoPath != "" {
		if _, err := os.Stat(videoPath); err != nil {
			videoPath = ""
		}
	}
	if audioPath != "" {
		if _, err := os.Stat(audioPath); err != nil {
			audioPath = ""
		}
	}

	if videoPath == "" && audioPath == "" {
		return nil, fmt.Errorf("no media files to mux for segment %d", segIdx)
	}

	// MuxCopy (no re-encoding)
	if err := o.mux().MuxCopy(ctx, videoPath, audioPath, outputPath); err != nil {
		return nil, fmt.Errorf("mux segment %d: %w", segIdx, err)
	}

	// FFprobe for metadata
	probeData := o.runFFprobe(ctx, outputPath)

	// ENGINE-9, part edition: a part whose copy stopped early must not be
	// persisted as a finished segment. Returning the error before AddSegment
	// leaves the index unrecorded, which is what keeps its staging dir alive
	// (hasUnmuxedPartsForJob) for a re-mux — and the short part file is
	// removed with it, so the re-mux writes the real part at that name rather
	// than beside an unreferenced truncated twin of it.
	if probeData != nil {
		if err := o.verifyMuxedDuration(ctx, jobCtx.Job.ID, probeData.DurationSec, videoPath, audioPath); err != nil {
			o.discardShortMuxOutput(jobCtx.Job.ID, outputPath)
			return nil, fmt.Errorf("mux segment %d: %w", segIdx, err)
		}
	}

	// Resolve the unixStart sentinel (see the doc comment): derive the start
	// from the probed duration, clamped so it never precedes the job's
	// download start — mirroring muxUnrecordedSegments' recovery heuristic.
	if unixStart == 0 {
		unixStart = unixEnd
		if probeData != nil && probeData.DurationSec > 0 {
			unixStart = unixEnd - int64(probeData.DurationSec)
		}
		if ds := jobCtx.Job.DownloadStartedAt; ds != "" {
			if t, perr := time.Parse(time.RFC3339, ds); perr == nil && unixStart < t.Unix() {
				unixStart = t.Unix()
			}
		}
	}

	// Get file info
	info, _ := os.Stat(outputPath)

	// Build segment record
	seg := &database.Segment{
		JobID:        jobCtx.Job.ID,
		SegmentIndex: segIdx,
		UnixStart:    unixStart,
		UnixEnd:      unixEnd,
		Quality:      quality.Label,
		Filename:     segFilename,
		FilePath:     outputPath,
	}

	if info != nil {
		size := info.Size()
		seg.FileSize = &size
	}
	if probeData != nil {
		if probeData.Width > 0 {
			seg.VideoWidth = &probeData.Width
		}
		if probeData.Height > 0 {
			seg.VideoHeight = &probeData.Height
		}
		if probeData.Fps > 0 {
			seg.VideoFps = &probeData.Fps
		}
		seg.DurationSeconds = probeData.DurationSec
	}

	// Per-part chat (Twitch): the rolled chat file for this capture span is
	// copied beside the part video and recorded on the segment row.
	if result.ChatPath != "" {
		if _, statErr := os.Stat(result.ChatPath); statErr == nil {
			chatDst := filepath.Join(outputDir, partBase+".chat.json")
			if copyErr := copyFile(result.ChatPath, chatDst); copyErr != nil {
				o.logger.Warn("failed to copy part chat file", "err", copyErr, "segment", segIdx)
			} else {
				seg.ChatFile = chatDst
			}
		}
	}

	// Persist to database
	if err := o.db.AddSegment(seg); err != nil {
		o.logger.Error("failed to persist segment", "err", err, "segment", segIdx)
		return seg, fmt.Errorf("persist segment: %w", err)
	}

	return seg, nil
}

// muxFromStaging discovers segment files in the staging directory and runs
// the full mux pipeline. Used for the "Mux" action on cancelled/errored jobs
// where no DownloadResult exists from the download pipeline.
func (o *DownloadOrchestrator) muxFromStaging(ctx context.Context, jobCtx *JobContext) error {
	stagingDir := jobCtx.StagingDir

	// Recover quality-split segment dirs (seg_N) that were never muxed —
	// e.g. the job errored or was cancelled after a split. Without this,
	// muxAndFinalize would see the existing DB segments and finalize while
	// silently dropping the newest segment's data.
	o.muxUnrecordedSegments(ctx, jobCtx)

	// Discover segment files in priority order (DASH > HLS > VOD)
	result := discoverStagingMedia(stagingDir)
	if result == nil {
		// No root media. A post-split job keeps its newest data in seg_N —
		// if the recovery above (or earlier background muxes) produced DB
		// segments, finalize from those.
		if segments, err := o.db.GetSegments(jobCtx.Job.ID); err == nil && len(segments) > 0 {
			return o.finalizeMultiSegmentJob(ctx, jobCtx, segments)
		}
		return fmt.Errorf("no segment files found in staging directory")
	}

	return o.muxAndFinalize(ctx, jobCtx, result)
}

// discoverStagingMedia returns a DownloadResult pointing at the recognized
// media files inside dir (DASH > HLS > VOD priority), or nil when dir holds
// no media.
func discoverStagingMedia(dir string) *DownloadResult {
	result := &DownloadResult{}

	// DASH segments
	if fileExists(filepath.Join(dir, "video_stream")) {
		result.VideoPath = filepath.Join(dir, "video_stream")
		result.HasVideo = true
	}
	if fileExists(filepath.Join(dir, "audio_stream")) {
		result.AudioPath = filepath.Join(dir, "audio_stream")
		result.HasAudio = true
	}

	// HLS (single muxed stream)
	if !result.HasVideo && fileExists(filepath.Join(dir, "video.ts")) {
		result.VideoPath = filepath.Join(dir, "video.ts")
		result.HasVideo = true
		result.IsHls = true
	}

	// VOD
	if !result.HasVideo && fileExists(filepath.Join(dir, "video.mp4")) {
		result.VideoPath = filepath.Join(dir, "video.mp4")
		result.HasVideo = true
	}
	if !result.HasAudio && fileExists(filepath.Join(dir, "audio.m4a")) {
		result.AudioPath = filepath.Join(dir, "audio.m4a")
		result.HasAudio = true
	}

	if !result.HasVideo && !result.HasAudio {
		return nil
	}
	return result
}

// stagedSeg is one seg_N staging dir mapped to its part index.
type stagedSeg struct {
	idx int
	dir string
}

// mergeTombstoneFile marks a superseded part's pre-mux staging dir as
// already consumed by a Tier 4 same-format merge (part_merge.go's merge
// method writes it, BEFORE attempting RemoveAll on that dir, once
// db.ReplaceJobSegments has committed the merged row). A crash before the
// RemoveAll completes, or a locked-dir failure (Windows antivirus/indexer
// holding a handle), then leaves the raw pre-mux media behind — without
// this marker, stagedSegDirs would report that dir as a normal unmuxed
// part, and a later finalize re-entry could resurrect + re-persist +
// re-merge content that's already inside the merged output (duplication),
// or discoverResumeSegment could capture a live resume INTO a dir whose
// content the merge has already superseded. The marker file's own content
// is diagnostic only (a timestamp) — its mere presence is what matters.
const mergeTombstoneFile = ".merged-tombstone"

// isMergeTombstoned reports whether dir carries mergeTombstoneFile.
func isMergeTombstoned(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, mergeTombstoneFile))
	return err == nil
}

// stagedSegDirs scans stagingDir for seg_N part dirs and returns them sorted
// by index. This is THE mapping between the staging layout and part indices
// — startup resume (discoverResumeSegment) and finalize recovery
// (muxUnrecordedSegments, hasUnmuxedPartsForJob) all build on it; if they
// ever disagreed, a resumed job could append into a dir that recovery
// attributes to an already-muxed part. The staging ROOT is part index 0
// unless seg_0 exists (a short-skipped root span); that rule lives at the
// call sites.
//
// A tombstoned dir (isMergeTombstoned) is skipped entirely — it never
// appears in the returned slice at all, for any of the three consumers
// (I7 fix): its content has already been folded into a Tier 4 merge and
// the dir is pending (or has already failed) removal, so treating it as
// live staging would resurrect superseded content.
func stagedSegDirs(stagingDir string) []stagedSeg {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return nil
	}
	var segDirs []stagedSeg
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "seg_") {
			continue
		}
		n, convErr := strconv.Atoi(strings.TrimPrefix(e.Name(), "seg_"))
		if convErr != nil || n < 0 {
			continue
		}
		dir := filepath.Join(stagingDir, e.Name())
		if isMergeTombstoned(dir) {
			continue
		}
		segDirs = append(segDirs, stagedSeg{idx: n, dir: dir})
	}
	sort.Slice(segDirs, func(i, j int) bool { return segDirs[i].idx < segDirs[j].idx })
	return segDirs
}

// muxUnrecordedSegments muxes any part staging dir whose index has no row in
// the segments table. Times and quality are derived post-hoc: the media
// file's mtime approximates the segment end, and an ffprobe of the raw
// stream supplies dimensions and duration. Best-effort — individual
// failures are logged and skipped so the rest of the recovery proceeds.
func (o *DownloadOrchestrator) muxUnrecordedSegments(ctx context.Context, jobCtx *JobContext) {
	segDirs := stagedSegDirs(jobCtx.StagingDir)
	if len(segDirs) == 0 {
		return // no part splits — nothing to recover
	}

	recorded := map[int]bool{}
	segments, err := o.db.GetSegments(jobCtx.Job.ID)
	if err != nil {
		// Can't tell what's already muxed — bail rather than risk duplicate
		// segment rows; muxAndFinalize degrades the same way on this error.
		o.logger.Warn("segment recovery: failed to read segments", "err", err, "jobID", jobCtx.Job.ID)
		return
	}
	for _, s := range segments {
		recorded[s.SegmentIndex] = true
	}

	// Root staging files are segment 0 — unless seg_0 exists, in which case
	// the root data was a short segment the pipeline deliberately skipped.
	if segDirs[0].idx != 0 && !recorded[0] && discoverStagingMedia(jobCtx.StagingDir) != nil {
		segDirs = append([]stagedSeg{{idx: 0, dir: jobCtx.StagingDir}}, segDirs...)
	}

	// Per-part chat detection: when ANY seg_N dir carries its own chat.json,
	// the per-part rolling scheme was active for this job, and the root
	// chat.json (if present) is part 0's closed chat rather than a legacy
	// whole-job file. Without any seg-dir chat the root file stays the
	// whole-job chat handled by finalize's copyAssets.
	perPartChat := false
	for _, sd := range segDirs {
		if sd.dir != jobCtx.StagingDir && fileExists(filepath.Join(sd.dir, "chat.json")) {
			perPartChat = true
			break
		}
	}

	for _, sd := range segDirs {
		if recorded[sd.idx] {
			continue
		}
		media := discoverStagingMedia(sd.dir)
		if media == nil {
			continue
		}
		if chatPath := filepath.Join(sd.dir, "chat.json"); perPartChat && fileExists(chatPath) {
			media.ChatPath = chatPath
		}
		mediaPath := media.VideoPath
		if mediaPath == "" {
			mediaPath = media.AudioPath
		}
		unixEnd := time.Now().Unix()
		if info, statErr := os.Stat(mediaPath); statErr == nil {
			unixEnd = info.ModTime().Unix()
		}
		quality := QualityInfo{Label: "unknown"}
		if probe := o.runFFprobe(ctx, mediaPath); probe != nil && probe.Height > 0 {
			quality = QualityInfo{
				Width:  probe.Width,
				Height: probe.Height,
				FPS:    probe.Fps,
				Label:  FormatQualityLabel(probe.Height, probe.Fps),
			}
		}
		// unixStart 0 = muxSegment's derive-from-duration sentinel: it
		// computes end - probed duration (from the MUXED output — at least as
		// accurate as this raw stream's container metadata), clamped so the
		// span never precedes the job's download start.
		if seg, muxErr := o.muxSegment(ctx, jobCtx, sd.idx, 0, unixEnd, quality, media); muxErr != nil {
			o.logger.Error("failed to mux recovered segment", "segment", sd.idx, "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("recovered unmuxed quality segment", "segment", sd.idx, "file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

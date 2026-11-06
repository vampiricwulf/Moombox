package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// The fill's half of the archive-VOD gap-fill (owner decision D17; the
// capture's half is twitch_gapfill_plan.go). Nothing waits on it: the live job
// finalizes as recorded at once, and its plan is kept in twitch_gap_fills.
// The worker looks at each plan on a backoff until the broadcast's archive VOD
// is complete — listed, VideoMetadata no longer recording, its playlist ending
// in #EXT-X-ENDLIST — or until gapFillWindow after the broadcast ended (follow-
// up 4). It then cuts each span the capture lacks out of the VOD, on segment
// boundaries, muxes it as a part of its own beside the archive, and re-finalizes
// the job with the parts in time order (follow-up 1). The recorded footage is
// never touched: a fill that fails anywhere removes what it wrote and leaves the
// archive exactly as it was, and a VOD that never comes — deleted, never
// published, subscriber-only without access — leaves it as recorded, with one
// log line saying why.

// twitchGapFillPlan is one twitch_gap_fills row's plan.
type twitchGapFillPlan struct {
	JobID       string `json:"jobId"`
	Login       string `json:"login"`
	StreamStart string `json:"streamStart"` // RFC3339: the broadcast's createdAt, the VOD's offset 0
	EndedAt     string `json:"endedAt"`     // RFC3339: when the capture finalized
	Deadline    string `json:"deadline"`    // RFC3339: EndedAt + gapFillWindow
	// The selection the capture ran on, so the fill takes the same rendition
	// of the VOD: its variant's name first, else the same rule.
	VariantName   string               `json:"variantName,omitempty"`
	QualityPref   string               `json:"qualityPref,omitempty"`
	MaxResolution int                  `json:"maxResolution,omitempty"`
	Prefer60fps   bool                 `json:"prefer60fps,omitempty"`
	Parts         []stagedPartCoverage `json:"parts"`
	Spans         []gapSpan            `json:"spans"`
	VodID         string               `json:"vodId,omitempty"`
	Attempts      int                  `json:"attempts,omitempty"`
	LastReason    string               `json:"lastReason,omitempty"`
}

// gapFillWindow is how long after the broadcast ends the fill waits for its
// archive VOD (owner decision D17 follow-up 4: up to 24 h).
const gapFillWindow = 24 * time.Hour

// gapFillFirstCheck is the wait after a look found the VOD not ready, the
// first being the one the finished capture asks for at once
// (keepTwitchGapFillPlan): Twitch appends ENDLIST to a finished broadcast's
// archive within minutes, and its last segments are listed a little after the
// live edge's.
const gapFillFirstCheck = 10 * time.Minute

// gapFillBackoff is the wait before the next look after the n-th (1-based)
// look found the VOD not ready: 10 min doubling, capped at three hours, so a
// VOD that is never coming costs about a dozen looks across the window.
func gapFillBackoff(attempts int) time.Duration {
	d := gapFillFirstCheck
	for i := 1; i < attempts && d < 3*time.Hour; i++ {
		d *= 2
	}
	return min(d, 3*time.Hour)
}

// gapFillPad widens each span on both sides before it is mapped onto the
// VOD's segments: the live program date-times and the VOD's offset from the
// broadcast's createdAt agree to within a second or so, and the owner's rule
// is a small overlap over a hole (D17, timestamp mapping).
const gapFillPad = time.Second

// gapFillSegmentAttempts bounds the fetch of one VOD segment.
const gapFillSegmentAttempts = 3

// newTwitchGapFillPlan builds the plan for a live capture that finalized at
// now, or nil when it lacks nothing (or cannot be placed: no stream start).
func newTwitchGapFillPlan(job *database.Job, variant *TwitchVariantInfo, parts []stagedPartCoverage, tailOpen bool, now time.Time) *twitchGapFillPlan {
	start, err := time.Parse(time.RFC3339, job.StreamStartTime)
	if err != nil || variant == nil || variant.ChannelLogin == "" {
		return nil
	}
	spans := gapSpansFor(start, parts, variant.GapFillAdBreaks, tailOpen)
	if len(spans) == 0 {
		return nil
	}
	return &twitchGapFillPlan{
		JobID:         job.ID,
		Login:         variant.ChannelLogin,
		StreamStart:   start.UTC().Format(time.RFC3339),
		EndedAt:       now.UTC().Format(time.RFC3339),
		Deadline:      now.Add(gapFillWindow).UTC().Format(time.RFC3339),
		VariantName:   variant.Name,
		QualityPref:   variant.QualityPref,
		MaxResolution: variant.MaxResolution,
		Prefer60fps:   variant.Prefer60fps,
		Parts:         parts,
		Spans:         spans,
	}
}

// saveTwitchGapFillPlan stores plan for its next look at next.
func saveTwitchGapFillPlan(db *database.Database, plan *twitchGapFillPlan, next time.Time) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return db.SaveGapFill(database.GapFill{JobID: plan.JobID, Plan: string(data), NextCheckAt: next.UTC().Format(time.RFC3339)})
}

// twitchGapFiller runs plans. Its seams are production's Twitch service, HTTP
// client and muxer (newTwitchGapFiller); a test replaces them.
type twitchGapFiller struct {
	db     *database.Database
	logger logger

	// findVod is twitch.API.ArchiveVodBetween: the oldest ARCHIVE published
	// in [start, end), "" when there is none.
	findVod func(ctx context.Context, login string, start, end time.Time) (string, error)
	// vodInfo is twitch.Service.GetVodInfo.
	vodInfo func(ctx context.Context, vodID string) (*twitch.TwitchVodInfo, error)
	// vodVariants is twitch.Service.GetVodHLSPlaylist.
	vodVariants func(ctx context.Context, vodID string) ([]twitch.TwitchHLSVariant, error)
	// fetch GETs a playlist or a segment.
	fetch func(ctx context.Context, url string) ([]byte, error)
	// mux turns the cut span's MPEG-TS into the part's MP4.
	mux func(ctx context.Context, tsPath, outPath string) error
	// diskCritical reports the output volume at the disk_critical threshold
	// (production: DownloadWorker.readOutputDisk's reading, the one the
	// backlog scheduler's admission gate closes on); nil never is.
	diskCritical func() bool
	// archiveRewriting reports a chapter import holding the job's claim
	// (production: DownloadWorker.chapterImportRunning); nil never is.
	archiveRewriting func(jobID string) bool
}

// newTwitchGapFiller is the production filler.
func newTwitchGapFiller(db *database.Database, tw *twitch.Service, o *DownloadOrchestrator, lg logger) *twitchGapFiller {
	f := &twitchGapFiller{db: db, logger: lg, fetch: gapFillFetch}
	if tw != nil {
		f.findVod = func(ctx context.Context, login string, start, end time.Time) (string, error) {
			return tw.API.ArchiveVodBetween(ctx, login, start, end, tw.Auth.GetAuthToken())
		}
		f.vodInfo = tw.GetVodInfo
		f.vodVariants = tw.GetVodHLSPlaylist
	}
	if o != nil {
		f.mux = func(ctx context.Context, tsPath, outPath string) error {
			return o.mux().MuxCopy(ctx, tsPath, "", outPath)
		}
	}
	return f
}

// newTwitchGapFiller is the worker's filler: newTwitchGapFiller over its
// database, Twitch service and orchestrator, with the disk gate read off the
// same output-volume reading the backlog scheduler admits on.
func (w *DownloadWorker) newTwitchGapFiller() *twitchGapFiller {
	f := newTwitchGapFiller(w.db, w.tw, w.orchestrator, w.logger)
	f.diskCritical = func() bool {
		r, err := w.readOutputDisk()
		return err == nil && r.critical
	}
	f.archiveRewriting = w.chapterImportRunning
	return f
}

// gapFillFetch GETs url with the worker's HTTP client.
func gapFillFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := workerHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{StatusCode: resp.StatusCode}
	}
	return io.ReadAll(resp.Body)
}

// gapFillPollInterval is how often the worker looks for a plan that is due.
const gapFillPollInterval = 5 * time.Minute

// runTwitchGapFills sweeps the due plans every gapFillPollInterval until ctx
// ends, the first tick one interval after start (past the boot's cleanup and
// its burst of requests), and at once whenever a finished capture stores a
// plan (the orchestrator's gapFillKick), so a VOD that is already complete is
// filled without waiting for a tick. A panic in one plan's look is recovered
// in sweep (stepRecovered), so this loop outlives it; the recover here is the
// goroutine's own backstop.
func (w *DownloadWorker) runTwitchGapFills(ctx context.Context, f *twitchGapFiller) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in the Twitch gap-fill sweep", "panic", fmt.Sprint(r))
		}
	}()
	var kick <-chan struct{}
	if w.orchestrator != nil {
		kick = w.orchestrator.gapFillKick
	}
	t := time.NewTicker(gapFillPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		f.sweep(ctx, time.Now())
	}
}

// sweep looks at every plan due at now, one after another.
func (f *twitchGapFiller) sweep(ctx context.Context, now time.Time) {
	rows, err := f.db.DueGapFills(now.UTC().Format(time.RFC3339))
	if err != nil {
		f.logger.Warn("gap-fill: could not read the due plans", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		f.stepRecovered(ctx, row, now)
	}
}

// stepRecovered is step with a recover of its own. A plan whose look panicked
// is dropped — the archive kept as recorded — rather than left due, where it
// would panic again at the next sweep and at every boot after; the sweep goes
// on to the next plan.
func (f *twitchGapFiller) stepRecovered(ctx context.Context, row database.GapFill, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			f.logger.Error("panic in a Twitch gap-fill; its plan is dropped and the archive kept as recorded",
				"jobID", row.JobID, "panic", fmt.Sprint(r))
			_ = f.db.DeleteGapFill(row.JobID)
		}
	}()
	f.step(ctx, row, now)
}

// step takes one look at a plan: it fills the job, or reschedules the plan on
// the backoff, or — past the deadline, or with the job gone or no longer
// Finished — drops it, keeping the archive as recorded.
func (f *twitchGapFiller) step(ctx context.Context, row database.GapFill, now time.Time) {
	var plan twitchGapFillPlan
	if err := json.Unmarshal([]byte(row.Plan), &plan); err != nil {
		f.logger.Warn("gap-fill: unreadable plan dropped; the archive is kept as recorded", "jobID", row.JobID, "err", err)
		_ = f.db.DeleteGapFill(row.JobID)
		return
	}
	job, err := f.db.GetJob(plan.JobID)
	if err != nil {
		return // read again at the next sweep
	}
	if job == nil || job.Status != database.StatusFinished {
		f.logger.Info("gap-fill: the job is gone or no longer finished; nothing filled", "jobID", plan.JobID)
		_ = f.db.DeleteGapFill(plan.JobID)
		return
	}
	if f.archiveRewriting != nil && f.archiveRewriting(plan.JobID) {
		// A chapter import is rewriting the archive the fill would add parts
		// beside and refit the chapters of (owner decision PR-M5): the plan
		// is left as it is, due, and the next sweep looks again.
		f.logger.Info("gap-fill: a chapter import is rewriting the archive; looking again at the next sweep", "jobID", plan.JobID)
		return
	}
	deadline, _ := time.Parse(time.RFC3339, plan.Deadline)
	if !deadline.IsZero() && now.After(deadline) {
		reason := plan.LastReason
		if reason == "" {
			reason = "no complete archive VOD was found"
		}
		f.logger.Info("gap-fill: keeping the archive as recorded — "+reason+" within 24 hours of the broadcast's end",
			"jobID", plan.JobID, "spans", len(plan.Spans))
		_ = f.db.DeleteGapFill(plan.JobID)
		return
	}
	filled, reason := f.fill(ctx, &plan, job)
	if ctx.Err() != nil {
		return
	}
	if filled {
		_ = f.db.DeleteGapFill(plan.JobID)
		return
	}
	plan.Attempts++
	plan.LastReason = reason
	next := now.Add(gapFillBackoff(plan.Attempts))
	if !deadline.IsZero() && next.After(deadline) {
		next = deadline.Add(time.Second)
	}
	f.logger.Debug("gap-fill: the archive VOD is not ready; looking again later",
		"jobID", plan.JobID, "reason", reason, "next", next.UTC().Format(time.RFC3339))
	if err := saveTwitchGapFillPlan(f.db, &plan, next); err != nil {
		f.logger.Warn("gap-fill: could not reschedule the plan", "jobID", plan.JobID, "err", err)
	}
}

// vodSegmentOffsets is each VOD segment's start, in seconds from the VOD's
// beginning, by its EXTINF durations; the last entry is the VOD's end.
func vodSegmentOffsets(segs []engine.HlsSegment) []float64 {
	out := make([]float64, len(segs)+1)
	for i, s := range segs {
		out[i+1] = out[i] + s.Duration
	}
	return out
}

// vodSegmentRange maps [fromSec, toSec) of the VOD onto its segments: the
// first segment ending after fromSec through the last one starting before
// toSec, so the cut covers the whole range and overlaps it by at most a
// segment at each end — trimmed on segment boundaries, never re-encoded.
// ok is false when the VOD holds none of the range.
func vodSegmentRange(offsets []float64, fromSec, toSec float64) (first, last int, ok bool) {
	n := len(offsets) - 1
	first, last = -1, -1
	for i := range n {
		if offsets[i+1] > fromSec && offsets[i] < toSec {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last, first >= 0
}

// recoveredPart is one span the fill cut and muxed, before it is committed.
// refused counts the segments left out of it because the VOD will not serve
// them (refusedVodSegment), refusedSec their length.
type recoveredPart struct {
	span       gapSpan
	path       string
	size       int64
	duration   float64
	muted      int
	refused    int
	refusedSec float64
}

// gapFillTimelineSlack is how much shorter than the capture's own covered
// stretch the archive VOD may be before its timeline is distrusted: the VOD's
// last segments trail the live edge's by some seconds, and the capture's
// program date-times and the VOD's createdAt-based offsets agree only to
// within a second or so.
const gapFillTimelineSlack = time.Minute

// coveredUntilMs is the latest end any part's coverage records (epoch ms), 0
// for none.
func coveredUntilMs(parts []stagedPartCoverage) int64 {
	var end int64
	for _, p := range parts {
		end = max(end, p.Cov.EndMs)
	}
	return end
}

// errSpanRefused is cutSpan's answer when the VOD refused every segment of
// the span: nothing to insert, and no reason to look again.
var errSpanRefused = errors.New("the archive VOD refuses every segment of the span")

// refusedVodSegment reports a fetch error that is the VOD's lasting answer
// for that segment rather than a passing failure: a 403, 404 or 410 that
// held through gapFillSegmentAttempts tries. On Twitch's VOD CDN a lasting
// 403 is a muted part that neither yt-dlp nor streamlink can fetch either —
// the engine's VOD downloader skips it as a gap and completes the VOD
// (fetchSegmentWithRetry) — so the fill leaves it out of the span, logged,
// and inserts the rest (owner decision D17 follow-up 2: use what the VOD
// has, and say so). It is yt-dlp's skip_unavailable_fragments, on by default
// (FragmentFD, yt_dlp/downloader/fragment.py b77c9304e: a fragment that
// fails every retry is skipped and the download goes on), narrowed to the
// answers that are lasting: a transport error or a 5xx is a reason to look
// again later, not a segment to leave out.
func refusedVodSegment(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false
	}
	switch he.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// fill tries the plan once. It reports filled when the job was re-finalized
// with the recovered parts — or when there was nothing the VOD could supply —
// and otherwise the reason the VOD is not ready (the plan stays).
func (f *twitchGapFiller) fill(ctx context.Context, plan *twitchGapFillPlan, job *database.Job) (bool, string) {
	start, err := time.Parse(time.RFC3339, plan.StreamStart)
	if err != nil {
		f.logger.Warn("gap-fill: the plan has no stream start; the archive is kept as recorded", "jobID", plan.JobID)
		return true, ""
	}
	if f.findVod == nil || f.vodInfo == nil || f.vodVariants == nil || f.fetch == nil || f.mux == nil {
		return false, "no Twitch service to read the archive VOD with"
	}
	// A fill writes each span twice over (its TS, then its MP4) onto the
	// archive's volume — a late start of hours is tens of GB — so it waits,
	// as the backlog does, while that volume is at disk_critical.
	if f.diskCritical != nil && f.diskCritical() {
		return false, "the output volume is critically full"
	}
	if plan.VodID == "" {
		end, _ := time.Parse(time.RFC3339, plan.EndedAt)
		id, err := f.findVod(ctx, plan.Login, start, end)
		if err != nil {
			return false, fmt.Sprintf("the archive VOD lookup failed (%v)", err)
		}
		if id == "" {
			return false, "no archive VOD of the broadcast is listed"
		}
		plan.VodID = id
	}
	info, err := f.vodInfo(ctx, plan.VodID)
	if err != nil {
		if errors.Is(err, twitch.ErrSubscriberOnlyNotEntitled) {
			return false, "the archive VOD is subscriber-only and this login has no access"
		}
		return false, fmt.Sprintf("the archive VOD %s could not be read (%v)", plan.VodID, err)
	}
	if info.IsRecording {
		return false, "the archive VOD is still recording"
	}
	variants, err := f.vodVariants(ctx, plan.VodID)
	if err != nil {
		if errors.Is(err, twitch.ErrSubscriberOnlyNotEntitled) {
			return false, "the archive VOD is subscriber-only and this login has no access"
		}
		return false, fmt.Sprintf("the archive VOD's playlist could not be read (%v)", err)
	}
	var pick *twitch.TwitchHLSVariant
	for i := range variants {
		if variants[i].Name == plan.VariantName {
			pick = &variants[i]
			break
		}
	}
	if pick == nil {
		pick = twitch.SelectBestVariant(variants, plan.QualityPref, plan.MaxResolution, plan.Prefer60fps)
	}
	if pick == nil {
		return false, "the archive VOD offers no rendition"
	}
	body, err := f.fetch(ctx, pick.URL)
	if err != nil {
		return false, fmt.Sprintf("the archive VOD's media playlist could not be read (%v)", err)
	}
	parsed := engine.ParseHls(string(body), pick.URL)
	if parsed == nil || parsed.Playlist == nil || len(parsed.Playlist.Segments) == 0 {
		return false, "the archive VOD's media playlist is empty"
	}
	if !parsed.Playlist.EndList {
		return false, "the archive VOD's playlist is not final yet"
	}
	segs := parsed.Playlist.Segments
	offsets := vodSegmentOffsets(segs)

	// The mapping's premise is that the VOD's offset 0 is the broadcast's
	// createdAt and that the VOD runs on with the wall clock. A VOD shorter
	// than the stretch the capture itself covered breaks it — the VOD left
	// something out, or does not start where the broadcast did — and every
	// cut from it would hold the wrong footage, silently. Such a VOD is not
	// used.
	if covered := coveredUntilMs(plan.Parts) - start.UnixMilli(); covered > 0 {
		vodMs := int64(offsets[len(offsets)-1] * 1000)
		if vodMs+gapFillTimelineSlack.Milliseconds() < covered {
			f.logger.Info("gap-fill: the archive VOD is shorter than the capture's own recording, so its timeline cannot be placed; the archive is kept as recorded",
				"jobID", plan.JobID, "vodID", plan.VodID, "vodSec", math.Round(float64(vodMs)/1000), "capturedSec", math.Round(float64(covered)/1000))
			return true, ""
		}
	}

	outDir, base := gapFillOutputLocation(job)
	if outDir == "" {
		f.logger.Warn("gap-fill: the archive's location is unknown; it is kept as recorded", "jobID", plan.JobID)
		return true, ""
	}
	release := claimOutputStem(job.ID, filepath.Join(outDir, base+" - recovered"))
	defer release()

	var made []recoveredPart
	undo := func() {
		for _, p := range made {
			_ = os.Remove(p.path)
		}
	}
	startMs := start.UnixMilli()
	for _, sp := range plan.Spans {
		fromSec := float64(sp.FromMs-startMs)/1000 - gapFillPad.Seconds()
		toSec := offsets[len(offsets)-1]
		if sp.ToMs > 0 {
			toSec = float64(sp.ToMs-startMs)/1000 + gapFillPad.Seconds()
		}
		first, last, ok := vodSegmentRange(offsets, max(fromSec, 0), toSec)
		if !ok {
			f.logger.Info("gap-fill: the archive VOD does not reach a lost span; it stays a gap",
				"jobID", plan.JobID, "kind", sp.Kind, "fromSec", math.Round(fromSec), "toSec", math.Round(toSec))
			continue
		}
		part, err := f.cutSpan(ctx, segs[first:last+1], outDir, base, len(made)+1)
		if errors.Is(err, errSpanRefused) {
			f.logger.Info("gap-fill: the archive VOD refuses every segment of a lost span; it stays a gap",
				"jobID", plan.JobID, "kind", sp.Kind, "vodFromSec", math.Round(offsets[first]), "vodToSec", math.Round(offsets[last+1]))
			continue
		}
		if err != nil {
			undo()
			return false, fmt.Sprintf("a span could not be cut from the archive VOD (%v)", err)
		}
		part.span = sp
		part.duration = offsets[last+1] - offsets[first] - part.refusedSec
		made = append(made, part)
		if part.refused > 0 {
			f.logger.Info("gap-fill: a recovered span leaves out VOD segments Twitch refuses to serve — a muted stretch neither tool can fetch",
				"jobID", plan.JobID, "kind", sp.Kind, "vodFromSec", math.Round(offsets[first]), "vodToSec", math.Round(offsets[last+1]),
				"refusedSegments", part.refused, "segments", last-first+1)
		}
		if part.muted > 0 {
			f.logger.Info("gap-fill: a recovered span uses muted VOD segments — its video is intact, its audio silent where Twitch muted it",
				"jobID", plan.JobID, "kind", sp.Kind, "vodFromSec", math.Round(offsets[first]), "vodToSec", math.Round(offsets[last+1]),
				"mutedSegments", part.muted, "segments", last-first+1)
		}
	}
	if len(made) == 0 {
		f.logger.Info("gap-fill: the archive VOD holds none of the lost spans; the archive is kept as recorded", "jobID", plan.JobID)
		return true, ""
	}
	if err := f.commit(plan, job, pick, made); err != nil {
		undo()
		return false, fmt.Sprintf("the recovered parts could not be recorded (%v)", err)
	}
	for _, p := range made {
		f.logger.Info("gap-fill: recovered a lost span from the archive VOD as its own part",
			"jobID", plan.JobID, "vodID", plan.VodID, "kind", p.span.Kind, "file", filepath.Base(p.path),
			"seconds", math.Round(p.duration))
	}
	return true, ""
}

// cutSpan fetches segs into one MPEG-TS beside the archive and muxes it into
// the recovered part's MP4 (base + " - recovered<k>.mp4", the first k not
// taken). The TS is removed either way; the MP4 only on failure.
func (f *twitchGapFiller) cutSpan(ctx context.Context, segs []engine.HlsSegment, outDir, base string, k int) (recoveredPart, error) {
	var part recoveredPart
	outPath := ""
	for n := k; ; n++ {
		p := filepath.Join(outDir, fmt.Sprintf("%s - recovered%d.mp4", base, n))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			outPath = p
			break
		}
	}
	tsPath := outPath + ".gapfill.ts"
	ts, err := os.Create(tsPath)
	if err != nil {
		return part, err
	}
	defer os.Remove(tsPath)
	writeErr := func() error {
		defer ts.Close()
		mapURI := ""
		for _, s := range segs {
			if s.MapURI != "" && s.MapURI != mapURI {
				init, err := f.fetchRetry(ctx, s.MapURI)
				if err != nil {
					return err
				}
				if _, err := ts.Write(init); err != nil {
					return err
				}
				mapURI = s.MapURI
			}
			data, err := f.fetchRetry(ctx, s.URL)
			if err != nil {
				if ctx.Err() == nil && refusedVodSegment(err) {
					part.refused++
					part.refusedSec += s.Duration
					continue
				}
				return err
			}
			if _, err := ts.Write(data); err != nil {
				return err
			}
			if isMutedVodSegment(s.URL) {
				part.muted++
			}
		}
		return nil
	}()
	if writeErr != nil {
		return part, writeErr
	}
	if part.refused == len(segs) {
		return part, errSpanRefused
	}
	if err := f.mux(ctx, tsPath, outPath); err != nil {
		_ = os.Remove(outPath)
		return part, err
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		return part, err
	}
	part.path, part.size = outPath, fi.Size()
	return part, nil
}

// isMutedVodSegment reports a segment Twitch muted for copyrighted audio: its
// URI ends -muted.ts ("123-muted.ts"), the form streamlink and yt-dlp both
// download as is.
func isMutedVodSegment(u string) bool {
	path, _, _ := strings.Cut(u, "?")
	return strings.HasSuffix(path, "-muted.ts") || strings.Contains(path, "-muted-")
}

// fetchRetry is fetch with up to gapFillSegmentAttempts tries.
func (f *twitchGapFiller) fetchRetry(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for range gapFillSegmentAttempts {
		data, err := f.fetch(ctx, url)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// gapFillOutputLocation is where the recovered parts go — beside the archive:
// the first part's directory and base for a split job, the archive file's for
// a single-file one — or "" when the row names neither.
func gapFillOutputLocation(job *database.Job) (dir, base string) {
	if len(job.Segments) > 0 && job.Segments[0].FilePath != "" {
		return filepath.Dir(job.Segments[0].FilePath), mergeBaseName(job.Segments[0].Filename)
	}
	if job.OutputFile != "" {
		return filepath.Dir(job.OutputFile), strings.TrimSuffix(filepath.Base(job.OutputFile), filepath.Ext(job.OutputFile))
	}
	return "", ""
}

// commit re-finalizes the job with the recovered parts: its parts — the
// recorded ones as they are, a single-file archive made its one recorded part
// — and the recovered ones in time order, renumbered, and the job's length,
// size and first file updated to match. Only while the job is still Finished.
func (f *twitchGapFiller) commit(plan *twitchGapFillPlan, job *database.Job, v *twitch.TwitchHLSVariant, made []recoveredPart) error {
	// The recorded parts are read fresh, and the job's status is checked by
	// ReplaceJobSegmentsIf below, in the transaction that writes them: an
	// operator's re-run, reset or delete that lands while the fill ran is
	// never written over.
	fresh, err := f.db.GetJob(job.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("the job changed while its gap-fill ran")
	}
	startOf := map[int]int64{}
	endOf := map[int]int64{}
	for _, p := range plan.Parts {
		startOf[p.Index] = p.Cov.StartMs
		endOf[p.Index] = p.Cov.EndMs
	}
	type keyed struct {
		key int64
		seg database.Segment
	}
	var all []keyed
	recorded := fresh.Segments
	if len(recorded) == 0 {
		if fresh.OutputFile == "" {
			return fmt.Errorf("the archive file is unknown")
		}
		seg := database.Segment{
			JobID:     fresh.ID,
			Quality:   fresh.TwitchQuality,
			Filename:  filepath.Base(fresh.OutputFile),
			FilePath:  fresh.OutputFile,
			FileSize:  fresh.FileSize,
			ChatFile:  fresh.ChatFile,
			UnixStart: startOf[0] / 1000,
			UnixEnd:   endOf[0] / 1000,
		}
		seg.VideoWidth, seg.VideoHeight, seg.VideoFps = fresh.VideoWidth, fresh.VideoHeight, fresh.VideoFps
		if fresh.LengthSeconds != nil {
			seg.DurationSeconds = float64(*fresh.LengthSeconds)
		}
		recorded = []database.Segment{seg}
	}
	for _, s := range recorded {
		key, ok := startOf[s.SegmentIndex]
		if !ok || key == 0 {
			key = s.UnixStart * 1000
		}
		all = append(all, keyed{key: key, seg: s})
	}
	fps := int(math.Round(v.FPS))
	for _, p := range made {
		size := p.size
		w, h := v.Width, v.Height
		seg := database.Segment{
			JobID:           job.ID,
			UnixStart:       p.span.FromMs / 1000,
			UnixEnd:         (p.span.FromMs + int64(p.duration*1000)) / 1000,
			Quality:         v.Name,
			Filename:        filepath.Base(p.path),
			FilePath:        p.path,
			FileSize:        &size,
			DurationSeconds: p.duration,
		}
		if w > 0 && h > 0 {
			seg.VideoWidth, seg.VideoHeight = &w, &h
		}
		if fps > 0 {
			seg.VideoFps = &fps
		}
		// A recovered span sorts just before the part that follows it: its
		// key is its own start, which the VOD's overlap can put a second
		// ahead of the end of the part before.
		all = append(all, keyed{key: p.span.FromMs + 1, seg: seg})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].key < all[j].key })
	segs := make([]database.Segment, len(all))
	var total float64
	var bytes int64
	for i, k := range all {
		k.seg.ID = 0
		k.seg.SegmentIndex = i
		segs[i] = k.seg
		total += k.seg.DurationSeconds
		if k.seg.FileSize != nil {
			bytes += *k.seg.FileSize
		}
	}
	applied, err := f.db.ReplaceJobSegmentsIf(job.ID, database.StatusFinished, segs)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("the job changed while its gap-fill ran")
	}
	// A split archive's chapter list (owner decision FE-15) was laid on the
	// parts as they stood; the recovered parts move every point after them.
	madePaths := make(map[string]bool, len(made))
	for _, p := range made {
		madePaths[p.path] = true
	}
	refitSplitArchiveChapters(recorded, segs, madePaths, f.logger)
	updates := map[string]any{"output_file": segs[0].FilePath, "length_seconds": int(total)}
	if bytes > 0 {
		updates["file_size"] = bytes
	}
	if !f.db.UpdateJobFieldsIf(job.ID, database.StatusFinished, updates) {
		// The parts are recorded; the row moved on in the instant between
		// the guarded replace and this write (a delete or a re-run). Its
		// own finalize rewrites these fields.
		f.logger.Warn("gap-fill: the job changed as its recovered parts were recorded", "jobID", job.ID)
	}
	return nil
}

// twitchChannelGapFillAdBreaks reports whether the configured Twitch channel
// login (matched without regard to case) fills its ad breaks
// (twitch_gap_fill_ad_breaks).
func twitchChannelGapFillAdBreaks(channels []config.ChannelConfig, login string) bool {
	for i := range channels {
		if channels[i].GetPlatform() == "twitch" && strings.EqualFold(channels[i].ID, login) {
			return channels[i].TwitchGapFillAdBreaks
		}
	}
	return false
}

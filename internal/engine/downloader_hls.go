package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// errHlsInitWrite marks an ensureHlsInit failure as a local WRITE error (disk
// full, handle revoked) rather than a network fetch failure. The distinction
// is load-bearing: fetch failures are retried across playlist rounds, but
// retrying a write re-issues the full init after whatever partial bytes the
// failed write left behind — a corrupt file head — so callers treat these as
// fatal, exactly like a failed media-segment write. (The partial bytes are
// harmless on the next resume: bytesWritten is not advanced on the failed
// write, so the sidecar's truncate-to-known-good drops them.)
var errHlsInitWrite = errors.New("write HLS init segment")

// ensureHlsInit makes the output file valid for the media segment about to be
// written under mapURI, the segment's governing #EXT-X-MAP init URI (fMP4/CMAF
// HLS — Twitch's TS→fMP4 migration). Raw fMP4 fragments are headerless
// (styp+moof+mdat, no ftyp/moov), so the init segment MUST be at the head of
// the file or ffmpeg cannot demux it at mux time. Callers invoke it only when
// the media segment's own data is already in hand, so the file never holds an
// init without media behind it — the StopOnGap escalations' "file is empty"
// discriminator keeps meaning "no media captured".
//
// A changed URI is compared by CONTENT hash, because Twitch URLs rotate
// tokens — URI equality alone would misread every rotation as a transcode
// restart. (An UNCHANGED URI is trusted without a re-fetch: init resources
// are immutable per RFC 8216 §6.3.4 — that assumption is what makes the
// per-segment fast path free.) Outcomes:
//   - TS segment, TS file → no-op
//   - nothing written yet → write the init at the head of the fresh file
//   - already written, identical bytes → adopt the new URI, write nothing
//   - content actually differs, the file predates fMP4 support (init state
//     unknown), or the playlist REVERTED to TS while the file starts with an
//     init: StopOnGap returns ErrInitSegmentChanged so the caller
//     part-splits; other configurations continue in-file with a loud Error
//     log — the tail may not survive a -c copy mux, but erroring out would
//     lose the rest of a VOD outright.
func (d *SegmentDownloader) ensureHlsInit(ctx context.Context, mapURI string) error {
	if mapURI == "" {
		if !d.hlsInitWritten {
			return nil // TS segment into a TS file
		}
		// fMP4 → TS reversion: the playlist stopped carrying EXT-X-MAP while
		// the file already starts with an init segment (e.g. Twitch rolling
		// its fMP4 experiment back mid-broadcast, or a post-outage variant
		// swap landing on a TS edge). Raw TS after ISOBMFF fragments makes
		// the tail undemuxable.
		if d.opts.StopOnGap {
			d.logger.Warn("[Downloader] HLS playlist reverted to TS mid-part, stopping for part split")
			return ErrInitSegmentChanged
		}
		d.logger.Error("[Downloader] HLS playlist reverted to TS mid-file; appending TS after fMP4 — mux may fail from this point")
		d.hlsInitWritten = false
		d.hlsInitURI = ""
		d.hlsInitHash = ""
		return nil
	}
	if mapURI == d.hlsInitURI {
		return nil // unchanged URI ⇒ unchanged init (immutability assumption above)
	}
	data, err := d.fetchSegmentWithRetry(ctx, mapURI, nil)
	if err != nil {
		return fmt.Errorf("fetch HLS init segment: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if d.hlsInitWritten && hash == d.hlsInitHash {
		// Token-rotated URI, same init — adopt the name, nothing to write.
		d.hlsInitURI = mapURI
		return nil
	}
	if d.hlsInitWritten || d.bytesWritten.Load() > 0 {
		// The file holds data under a DIFFERENT (or unknown) init.
		if d.opts.StopOnGap {
			d.logger.Warn("[Downloader] HLS init segment changed, stopping for part split",
				"newInit", truncateURL(mapURI, 120))
			return ErrInitSegmentChanged
		}
		// ffmpeg's mov demuxer skips a second moov, so fragments after this
		// point demux under the FIRST init's sample descriptions — the mux
		// may fail or misdecode from here. Writing the bytes anyway is the
		// least-loss fallback for a VOD; the Error level is what makes a
		// field failure diagnosable.
		d.logger.Error("[Downloader] HLS init segment changed mid-file, writing new init inline — mux may fail from this point",
			"newInit", truncateURL(mapURI, 120))
	}
	n, werr := d.outputFile.Write(data)
	if werr != nil {
		return fmt.Errorf("%w: %v", errHlsInitWrite, werr)
	}
	d.bytesWritten.Add(int64(n))
	d.hlsInitWritten = true
	d.hlsInitURI = mapURI
	d.hlsInitHash = hash
	d.logger.Info("[Downloader] HLS init segment written", "bytes", n,
		"uri", truncateURL(mapURI, 120))
	return nil
}

// waitOnline blocks until connectivity returns, then resets the no-segment
// clock so the offline stretch doesn't count toward the MaxTimeout backstop
// (which measures from lastSegTime at the top of runHlsLoop). EVERY HLS
// offline-recovery branch must route through this: otherwise a reconnect after
// an outage longer than MaxTimeout would trip the backstop and force-finalize a
// still-live recording — the outage branches unblock only once IsOnline() is
// true, so the backstop's own offline sub-branch never gets a chance to reset
// the clock first. Returns the context error if cancelled while waiting.
func (d *SegmentDownloader) waitOnline(ctx context.Context) error {
	if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
		return err
	}
	d.lastSegTime.StoreNow()
	return nil
}

// hlsReloadDelay returns how long to wait before the next media-playlist reload,
// porting ffmpeg's libavformat/hls.c pacing so the live loop hugs the live edge
// instead of accumulating segment batches. `elapsed` is the time already spent
// this cycle since the playlist was loaded (ffmpeg's last_load_time), so the
// result is the REMAINDER of the reload interval:
//
//   - hadNewSegments (flowing): interval = the last segment's duration —
//     ffmpeg default_reload_interval — falling back to targetDur, then 2s if the
//     playlist declared neither.
//   - !hadNewSegments (stalled at the live edge): interval = targetDur/2, matching
//     ffmpeg switching to half the target duration for still-empty reloads.
//   - clamp to 0 when already past the interval, so we reload immediately —
//     ffmpeg's `now - last_load_time >= reload_interval` (its wait `while` is then
//     false). The caught-up cadence is still floored by the interval, so this
//     never reloads faster than the segment rate once we've caught up.
func hlsReloadDelay(lastSegDur, targetDur float64, hadNewSegments bool, elapsed time.Duration) time.Duration {
	if targetDur <= 0 {
		targetDur = 2.0
	}
	var interval float64
	if hadNewSegments {
		interval = lastSegDur
		if interval <= 0 {
			interval = targetDur
		}
	} else {
		interval = targetDur / 2
	}
	remain := time.Duration(interval*float64(time.Second)) - elapsed
	if remain < 0 {
		remain = 0
	}
	return remain
}

// runHlsLoop is the main HLS download loop.
func (d *SegmentDownloader) runHlsLoop(ctx context.Context) error {
	// Save resume state on exit so interrupted downloads can continue on restart.
	// Only clear the resume file when the stream ends naturally.
	defer func() {
		d.saveResume()
		if d.streamEnded.Load() {
			d.ClearResume()
		}
	}()

	// Initialize the no-segment clock so the MaxTimeout backstop (YouTube HLS
	// only, see below) measures from loop entry rather than the zero time.
	d.lastSegTime.StoreNow()

	staleCount := 0
	consecutiveErrors := 0
	// Track retries for a single segment sequence so we don't spin forever
	// on a segment the CDN is returning 5xx for. When we hit the cap, we
	// escalate to the stream-status check path so the loop can exit if the
	// stream actually ended, or advance past the segment if not.
	stuckSeq := int64(-1)
	stuckSeqRetries := 0
	// lastSavedSeq tracks the currentSeq at the last resume-state save so the
	// per-iteration save can skip no-progress refreshes (see below). -1 forces
	// the first save.
	lastSavedSeq := -1
	// consecutiveStuckSkips bounds termination when EVERY segment fails
	// (expired auth, dead variant): each skip costs ~12s of retries, and an
	// ended stream with a long listed backlog would otherwise grind through
	// it skip by skip before EXT-X-ENDLIST/stale handling could fire.
	consecutiveStuckSkips := 0
	// initFetchFailures bounds retries of a transiently-unfetchable #EXT-X-MAP
	// init segment (fMP4 playlists only). Each failure already burned
	// fetchSegmentWithRetry's internal budget, so this counts playlist-refresh
	// ROUNDS; without an init the file is unwritable, so exhaustion escalates
	// through the stream-status check (ended → clean finalize; live →
	// ErrQualityLost for a variant refresh with a fresh init URI) instead of
	// spinning.
	initFetchFailures := 0
	// inAdBreak/adSegmentsSkipped track a Twitch stitched-ad run across
	// playlist refreshes so the break logs once at entry (Info) and once with
	// the total at exit, not per 2s segment.
	inAdBreak := false
	adSegmentsSkipped := 0
	// The break-exit log normally fires when the first post-ad content
	// segment is processed. When the loop exits DURING a break (stream ends
	// mid-ad, gap split, error, cancel) that site is never reached and the
	// skip tally would vanish — close the bookkeeping here instead. Captures
	// the variables by reference, so it sees their final values.
	defer func() {
		if inAdBreak {
			d.logger.Info("[Downloader] HLS loop exited during a Twitch stitched-ad break",
				"adSegmentsSkipped", adSegmentsSkipped)
		}
	}()

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Maximum-timeout backstop (YouTube HLS only — see EnforceMaxTimeout):
		// once the no-segment gap exceeds the configured MaxTimeout, force-
		// finalize even if YouTube still reports the stream live (its status can
		// lag or stick). Offline pauses the clock — wait for connectivity rather
		// than give up. The clock resets whenever a segment lands, so a recovered
		// stream gets a fresh budget. Twitch HLS leaves EnforceMaxTimeout false
		// and relies on its (reliable) GQL end-detection instead.
		if d.opts.EnforceMaxTimeout && d.lastSegTime.Since() >= d.opts.MaxTimeout {
			if d.opts.IsOnline != nil && !d.opts.IsOnline() {
				d.emitActivity(ActivityReconnecting)
				d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
				if err := d.waitOnline(ctx); err != nil {
					return err
				}
				continue
			}
			d.logger.Info("[Downloader] maximum timeout reached while waiting for segment; finalizing",
				"maxTimeout", d.opts.MaxTimeout, "gap", d.lastSegTime.Since().Round(time.Second))
			d.streamEnded.Store(true)
			return nil
		}

		// Fetch playlist
		data, plStatus, err := d.fetchSegment(ctx, d.getBaseURL())
		if err != nil {
			// 404/410 on playlist fetch -- variant may have been removed
			if plStatus == 404 || plStatus == 410 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := d.waitOnline(ctx); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				d.streamEnded.Store(true)
				return nil
			}
			consecutiveErrors++
			if consecutiveErrors > 5 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := d.waitOnline(ctx); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
				// Before giving up, check if stream is still live (quality may have changed)
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				return fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", consecutiveErrors, err)
			}
			d.emitActivity(ActivityRetrying)
			utils.Sleep(ctx, 5*time.Second)
			continue
		}

		result := ParseHls(string(data), d.getBaseURL())
		if result == nil || result.Playlist == nil {
			consecutiveErrors++
			// Log a snippet of the response so "parse failure" surfaces the
			// actual content — often CDNs return HTML error pages with
			// 200 OK that ParseHls silently rejects. First 200 bytes is
			// enough to tell m3u8 from HTML/JSON/text.
			snippet := string(data)
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			if strings.HasPrefix(strings.TrimSpace(snippet), "<") {
				d.logger.Warn("[Downloader] HLS playlist returned HTML instead of m3u8",
					"snippet", snippet, "attempt", consecutiveErrors)
			} else {
				d.logger.Debug("[Downloader] HLS playlist parse failed",
					"snippet", snippet, "attempt", consecutiveErrors)
			}
			if consecutiveErrors > 5 {
				// Mirror the fetch-error escalation above: garbage playlists
				// during an outage (router/captive-portal error pages served
				// with 200) must wait for connectivity, and a live stream
				// whose CDN serves transient garbage should re-resolve the
				// variant rather than hard-fail the recording.
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("playlist parse failures while device offline, waiting for connectivity")
					if err := d.waitOnline(ctx); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				return fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", consecutiveErrors)
			}
			d.emitActivity(ActivityRetrying)
			utils.Sleep(ctx, 5*time.Second)
			continue
		}
		pl := result.Playlist

		// consecutiveErrors counts playlist fetch/parse failures, so it resets
		// HERE — the playlist round-trip just succeeded — not at the bottom of
		// the iteration. The old end-of-iteration reset was skipped by the
		// segFailed continue, so a stretch of segment failures kept a stale
		// playlist-error count alive and a later fetch blip hit the >5
		// escalation early.
		consecutiveErrors = 0

		// ffmpeg hls.c stamps last_load_time at the end of parse_playlist; mirror
		// it here so the reload interval (hlsReloadDelay, at the tail) is measured
		// from when this playlist was loaded — the segment downloads below then
		// count against the interval instead of being added after a fixed sleep.
		loadTime := time.Now()

		// Initialize currentSeq if needed
		curSeq := int(d.currentSeq.Load())
		if curSeq < 0 {
			d.currentSeq.Store(int64(pl.MediaSequence))
			curSeq = pl.MediaSequence
		}

		// Handle gap: if currentSeq < mediaSequence, segments expired from CDN
		if curSeq < pl.MediaSequence {
			if d.opts.StopOnGap && d.bytesWritten.Load() > 0 {
				// The file on disk ends exactly where data stopped being
				// available — return without advancing currentSeq so the
				// caller can mux it as a gapless part and seed a fresh
				// downloader from CurrentSeq. Covers both mid-session CDN
				// gaps and the resume-after-restart case (resume state put
				// currentSeq at the old position; the first playlist fetch
				// reveals whether the window still covers it). OnGap is
				// deliberately NOT fired here: the successor downloader —
				// seeded at this position with an empty file — takes the
				// skip-forward branch below on its first fetch and records
				// the gap exactly once.
				d.logger.Warn("[Downloader] Unrecoverable gap in live playlist, stopping for part split",
					"from", curSeq, "to", pl.MediaSequence-1,
					"missing", pl.MediaSequence-curSeq)
				return ErrGapDetected
			}
			if d.OnGap != nil {
				d.OnGap(DownloadGap{From: curSeq, To: pl.MediaSequence - 1})
			}
			d.currentSeq.Store(int64(pl.MediaSequence))
			curSeq = pl.MediaSequence
		}

		// Identify new segments (only those >= currentSeq)
		var newSegments []HlsSegment
		for i, seg := range pl.Segments {
			segSeq := pl.MediaSequence + i
			if segSeq >= curSeq {
				newSegments = append(newSegments, seg)
			}
		}

		// VOD: parallel download only the filtered segments (not already downloaded).
		// StopOnGap recordings take the sequential path below even for the
		// final ENDLIST playlist: the VOD-parallel downloader skips
		// permanently-failed segments and keeps writing, which would bury a
		// discontinuity inside the final part — the sequential path's
		// stuck-segment escalation preserves the gapless-part contract. The
		// tail is at most one playlist window, so the lost parallelism is
		// seconds.
		if pl.EndList && len(newSegments) > 0 && !d.opts.StopOnGap {
			filteredPl := &HlsPlaylist{
				Segments:       newSegments,
				MediaSequence:  curSeq,
				EndList:        pl.EndList,
				TargetDuration: pl.TargetDuration,
			}
			return d.runHlsVodParallel(ctx, filteredPl)
		}

		// Live: download available segments
		segFailed := false
		for _, seg := range newSegments {
			if d.isCancelled() || ctx.Err() != nil {
				return d.cancelErr(ctx)
			}

			// Twitch stitched ad: server-side spliced ad media, not stream
			// content — during the break the real feed isn't served to this
			// token at all, so skipping loses nothing recordable. Advance the
			// position WITHOUT writing: currentSeq must keep pace with the
			// playlist window or the window sliding past the unadvanced seq
			// would read as a CDN gap and trigger a spurious part split. The
			// part file keeps an inline timestamp jump where the break was —
			// the same discontinuity a non-exempt viewer's player rides over.
			// Never fires for ad-free tokens or YouTube HLS (IsAd requires
			// twitch-stitched-ad DATERANGE markers; see collectAdDateRanges).
			if seg.IsAd {
				if !inAdBreak {
					inAdBreak = true
					d.logger.Info("[Downloader] Twitch stitched-ad break started; skipping ad segments",
						"seq", d.currentSeq.Load())
				}
				adSegmentsSkipped++
				d.currentSeq.Add(1)
				// The playlist is advancing normally — an ad break must not
				// trip the DASH-style no-segment liveness accounting.
				d.lastSegTime.StoreNow()
				continue
			}
			if inAdBreak {
				inAdBreak = false
				d.logger.Info("[Downloader] Twitch stitched-ad break ended",
					"adSegmentsSkipped", adSegmentsSkipped, "resumeSeq", d.currentSeq.Load())
				adSegmentsSkipped = 0
			}

			segData, segStatus, segErr := d.fetchSegment(ctx, seg.URL)
			if segErr != nil {
				// HLS 403s commonly indicate cipher rotation or POT expiry.
				// Fire OnCipherFailure once per downloader so the strategy can
				// invalidate cipher / POT / visitor-data caches; the next
				// orchestrator-driven refresh (quality monitor, stream-status
				// escalation) will then rebuild the variant URL with fresh
				// values. Mirrors the DASH path; HLS variant URL has POT in
				// its path so we can't hot-swap mid-loop, hence the empty
				// return is treated as a no-op here.
				if segStatus == 403 && d.cipherFailureFired.CompareAndSwap(false, true) &&
					d.OnCipherFailure != nil {
					if newURL := d.OnCipherFailure(); newURL != "" {
						d.SetBaseURL(newURL)
						d.logger.Info("[Cipher] HLS swapping BaseURL after 403",
							"url_prefix", truncateURL(newURL, 120))
					}
				}
				// Track repeated failures of the same sequence so we can
				// escalate when a permanently-unavailable segment is
				// stuck in the playlist.
				curSeqNow := d.currentSeq.Load()
				if curSeqNow == stuckSeq {
					stuckSeqRetries++
				} else {
					stuckSeq = curSeqNow
					stuckSeqRetries = 1
				}
				// Don't skip -- break to re-fetch playlist and retry.
				// If CDN purged it, gap detection handles it next iteration.
				d.logger.Debug("[Downloader] HLS segment failed, will retry after playlist refresh",
					"seq", curSeqNow, "error", segErr, "stuckRetries", stuckSeqRetries)
				if stuckSeqRetries >= MaxSegmentRetries {
					if d.opts.StopOnGap && d.bytesWritten.Load() > 0 {
						// Same contract as the playlist-window gap above: the
						// skipped segment would leave a discontinuity inside
						// the file, so end this part instead. currentSeq is
						// deliberately NOT advanced and OnGap NOT fired: the
						// resume sidecar must stay honest (LastSeq = last
						// byte actually in the file — advancing here let a
						// crash-resume append past a hole), and the SUCCESSOR
						// downloader — seeded at this position with an empty
						// file — retries the stuck segment once more and, if
						// it still fails, skips it via the empty-file rule,
						// recording the gap exactly once.
						d.logger.Warn("[Downloader] HLS segment permanently failing, stopping for part split",
							"seq", curSeqNow, "retries", stuckSeqRetries)
						return ErrGapDetected
					}
					// Skip past the stuck segment unconditionally — the
					// previous ended-check-first ordering abandoned the
					// still-listed tail when the stream happened to end
					// during the retries (the final window's segments stay
					// fetchable after the broadcast is over). With the skip,
					// an ended stream terminates a few iterations later via
					// EXT-X-ENDLIST or the stale-playlist escalation, both of
					// which fetch the remaining tail first.
					d.logger.Warn("[Downloader] HLS segment stuck, advancing past it as gap",
						"seq", curSeqNow, "retries", stuckSeqRetries)
					if d.OnGap != nil {
						d.OnGap(DownloadGap{From: int(curSeqNow), To: int(curSeqNow)})
					}
					d.currentSeq.Add(1)
					stuckSeq = -1
					stuckSeqRetries = 0
					consecutiveStuckSkips++
					// Several skips in a row without a single success means
					// nothing is fetchable — consult the stream status once
					// and exit cleanly if the broadcast is over, rather than
					// grinding through the remaining backlog. A localized
					// CDN hole resets the counter on the next good segment,
					// so a fetchable tail is still captured first.
					if consecutiveStuckSkips >= 3 && d.opts.CheckStreamStatus != nil {
						if ended, checkErr := d.opts.CheckStreamStatus(ctx); checkErr == nil && ended {
							d.logger.Warn("[Downloader] Stream ended and segments are unfetchable; stopping",
								"consecutiveSkips", consecutiveStuckSkips)
							d.streamEnded.Store(true)
							return nil
						}
					}
				}
				d.emitActivity(ActivityRetrying)
				utils.Sleep(ctx, 2*time.Second)
				segFailed = true
				break
			}
			// fMP4: the init segment governing this media segment must be at
			// the head of the file BEFORE the segment is written. Runs after
			// the media fetch SUCCEEDED so the file stays genuinely empty
			// until real media lands — the StopOnGap escalations' empty-file
			// rule (window gap above, stuck exhaustion below) keys on
			// bytesWritten, and an eagerly-written init used to flip it and
			// split-cycle init-only junk parts on a stuck first segment.
			// currentSeq has still not advanced, so a part split here leaves
			// the successor at this exact segment (its data is simply
			// re-fetched). Ad segments never reach this point (skipped
			// above), so an ad break's own init never counts as a change.
			if initErr := d.ensureHlsInit(ctx, seg.MapURI); initErr != nil {
				if errors.Is(initErr, ErrInitSegmentChanged) {
					return initErr
				}
				if errors.Is(initErr, errHlsInitWrite) {
					// Local write failure — fatal, like a failed media write
					// below; retrying would re-issue the init after partial
					// bytes and corrupt the file head.
					return initErr
				}
				if ctx.Err() != nil || d.isCancelled() {
					return d.cancelErr(ctx)
				}
				initFetchFailures++
				d.logger.Warn("[Downloader] HLS init segment fetch failed, will retry after playlist refresh",
					"error", initErr, "attempt", initFetchFailures)
				if initFetchFailures >= MaxSegmentRetries {
					// Escalate like the sibling playlist/segment paths: an
					// ended stream finalizes cleanly, a live one hands the
					// orchestrator an ErrQualityLost so the variant refresh
					// mints a fresh init URI — a terminal error here would
					// end a live recording over what is usually a rotten
					// token.
					if d.opts.CheckStreamStatus != nil {
						ended, checkErr := d.opts.CheckStreamStatus(ctx)
						if checkErr == nil && ended {
							d.streamEnded.Store(true)
							return nil
						}
						return ErrQualityLost
					}
					return fmt.Errorf("HLS init segment fetch failed after %d rounds: %w", initFetchFailures, initErr)
				}
				d.emitActivity(ActivityRetrying)
				utils.Sleep(ctx, 2*time.Second)
				segFailed = true
				break
			}
			initFetchFailures = 0

			// Success — reset stuck tracking. Record the arrival for the
			// fetched-bytes signal (the playlist fetch above deliberately
			// does not — it is not stream payload).
			d.noteFetch(len(segData))
			stuckSeq = -1
			stuckSeqRetries = 0
			consecutiveStuckSkips = 0
			hlsSeq := int(d.currentSeq.Load())
			n, writeErr := d.outputFile.Write(segData)
			if writeErr != nil {
				return fmt.Errorf("write HLS segment %d: %w", hlsSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq.Add(1)
			d.lastSegTime.StoreNow()

			if d.OnProgress != nil {
				// Seq reports the just-WRITTEN sequence — the same convention
				// as the DASH downloaders — so the persisted last_video_seq
				// means one thing regardless of strategy. (It used to report
				// post-increment here, which made restart seeding guess the
				// writer's convention and skip a segment when it guessed
				// wrong across an HLS<->DASH strategy flip.)
				d.OnProgress(DownloadProgress{
					Seq:   hlsSeq,
					Bytes: d.bytesWritten.Load(),
				})
			}
		}

		if segFailed {
			continue // Retry from playlist refresh
		}

		// Stale detection: no new segments available
		if len(newSegments) == 0 {
			staleCount++
			if staleCount >= 5 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := d.waitOnline(ctx); err != nil {
						return err
					}
					staleCount = 0
					continue
				}
				d.emitActivity(ActivityVerifyingEnd)
				if d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						d.streamEnded.Store(true)
						return nil
					}
				}
				// Stream is live but our position sits FAR beyond the
				// playlist's entire window: the media-sequence numbering
				// restarted under us (e.g. a resumed job whose channel ended
				// one broadcast and started another while we were down). The
				// margin matters — a lagging CDN replica can legitimately
				// serve a window a few segments behind our edge position for
				// several refreshes, and ending the recording on that
				// transient would permanently lose the rest of the stream.
				// A genuine numbering restart overshoots by hundreds of
				// segments, not a handful. End cleanly so the recording
				// muxes; the monitor picks the new broadcast up as a fresh
				// job.
				const seqRegressionMargin = 60 // ~2 min of 2s segments
				if curSeq > pl.MediaSequence+len(pl.Segments)+seqRegressionMargin {
					d.logger.Warn("[Downloader] HLS media sequence regressed far below resume position, ending recording",
						"curSeq", curSeq, "playlistEnd", pl.MediaSequence+len(pl.Segments))
					d.streamEnded.Store(true)
					return nil
				}
				// Reset so the next status probe waits another full stale
				// window instead of firing on every playlist refresh (~2s)
				// for as long as the stall lasts.
				staleCount = 0
			}
		} else {
			staleCount = 0
		}

		// Persist resume state only when the position actually advanced. The
		// loop iterates about once per reload interval (~one segment duration;
		// see hlsReloadDelay) and reaches here even on no-progress refreshes
		// (stale window, verifying-end); an unconditional saveResume there
		// fsync+renamed the sidecar every ~2s for zero recovery benefit (only
		// the timestamp changed, which the loader ignores within the 7-day
		// window). bytesWritten only
		// changes alongside currentSeq, so the seq is a complete progress
		// signal. The deferred saveResume still guarantees a final flush.
		if curSeqNow := int(d.currentSeq.Load()); curSeqNow != lastSavedSeq {
			d.saveResume()
			lastSavedSeq = curSeqNow
		}

		// Check if stream ended (EXT-X-ENDLIST present)
		if pl.EndList {
			// Natural end: mark ended so the deferred ClearResume removes the
			// sidecar (matches the DASH loop). Without this, streamEnded stays
			// false, the defer re-saves a fresh sidecar, and ClearResume is
			// skipped — leaving an orphaned .resume.json (masked today only
			// because the worker wipes staging, but a contract violation).
			d.streamEnded.Store(true)
			return nil
		}

		// Wait before the next reload — ffmpeg hls.c pacing (see hlsReloadDelay).
		// Measured from loadTime, so the segment downloads above count against the
		// interval rather than being added after a full fixed sleep (which is what
		// let segments batch up and the recording trail the live edge). Interval is
		// the last segment's duration while flowing, half the target duration while
		// stalled at the live edge; clamps to an immediate reload when already behind.
		var lastSegDur float64
		if n := len(pl.Segments); n > 0 {
			lastSegDur = pl.Segments[n-1].Duration
		}
		utils.Sleep(ctx, hlsReloadDelay(lastSegDur, pl.TargetDuration, len(newSegments) > 0, time.Since(loadTime)))
	}
}

// runHlsVodParallel downloads all VOD HLS segments in parallel with bounded concurrency.
// Uses a worker pool pattern: segmentWorkers() workers pull from a work channel,
// avoiding N goroutines sitting in memory for large VODs.
func (d *SegmentDownloader) runHlsVodParallel(ctx context.Context, pl *HlsPlaylist) error {
	totalSegs := len(pl.Segments)
	if totalSegs == 0 {
		return nil
	}

	type segWork struct {
		idx    int
		segURL string
	}
	type segResult struct {
		idx  int
		data []byte
	}

	// workers is the operative concurrency for this download (see
	// segmentWorkers) — computed once so channel sizing and the worker-pool
	// spawn below agree.
	workers := d.segmentWorkers()
	work := make(chan segWork, workers)
	results := make(chan segResult, workers*3)
	// done unblocks workers stuck on a full results channel when the
	// consumer below returns early on write error; without it wg.Wait
	// would never complete and the entire worker pool would leak.
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup

	// fetchItem downloads one segment, converting BOTH failure shapes —
	// fetch errors and panics — into the nil-data GAP SENTINEL. The
	// sentinel is load-bearing: every dequeued index must produce exactly
	// one result, or the consumer's nextIdx wedges on the missing index
	// while the other workers race the rest of the playlist into the
	// reorder buffer — accumulating the ENTIRE remaining VOD in RAM (a
	// multi-GB OOM on a long VOD). The per-item recover exists for the
	// same reason: a worker panic that only logged (the previous shape)
	// dropped its in-flight item's result and re-opened exactly that hole.
	fetchItem := func(item segWork) (data []byte) {
		defer func() {
			if r := recover(); r != nil {
				if d.logger != nil {
					d.logger.Error("VOD parallel download worker panic", "panic", r, "idx", item.idx)
				}
				data = nil // gap sentinel — the index must still emit a result
			}
		}()
		// No rebuild closure: HLS segment URLs are opaque literals lifted
		// from the playlist, not derived from d.getBaseURL()+seq, so a
		// refreshed base URL has nothing to rebuild here.
		data, fetchErr := d.fetchSegmentWithRetry(ctx, item.segURL, nil)
		if fetchErr != nil {
			// Audit reports/engine.md #17: distinguish CDN-evicted
			// segments from retries-exhausted so silent gaps are debuggable.
			switch {
			case errors.Is(fetchErr, ErrSegmentPermanent):
				d.logger.Debug("[Downloader] HLS VOD segment permanently gone (403/410)",
					"idx", item.idx)
			case errors.Is(fetchErr, ErrSegmentRetriesExhausted):
				d.logger.Debug("[Downloader] HLS VOD segment retries exhausted",
					"idx", item.idx)
			}
			return nil
		}
		return data
	}

	// Spawn fixed worker pool
	for range workers {
		wg.Go(func() {
			defer func() {
				// Backstop only (channel ops below): per-item panics are
				// already converted to sentinels inside fetchItem.
				if r := recover(); r != nil && d.logger != nil {
					d.logger.Error("VOD parallel download worker panic", "panic", r)
				}
			}()
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				select {
				case results <- segResult{idx: item.idx, data: fetchItem(item)}:
				case <-done:
					return
				}
			}
		})
	}

	// Feed work to workers. The send must select on done: when the consumer
	// below returns early (write error), the workers drain away and nothing
	// reads work anymore — a bare send would block this goroutine forever.
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("VOD feeder goroutine panic", "panic", r)
			}
		}()
		defer close(work)
		for i, seg := range pl.Segments {
			if d.isCancelled() || ctx.Err() != nil {
				return
			}
			select {
			case work <- segWork{idx: i, segURL: seg.URL}:
			case <-done:
				return
			}
		}
	}()

	// Close results when all workers done
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("panic in HLS VOD results closer", "panic", r)
			}
		}()
		wg.Wait()
		close(results)
	}()

	// Stream write: buffer out-of-order segments, write in order as they
	// arrive. Every index produces exactly one result (segment data or a
	// nil GAP SENTINEL from a failed worker), so the reorder buffer is
	// bounded by the in-flight window (~workers + results cap) —
	// a failed segment can no longer wedge nextIdx and accumulate the whole
	// VOD in RAM. gapStart coalesces consecutive missing indices into one
	// OnGap range and persists across the outer loop.
	buffer := make(map[int][]byte)
	nextIdx := 0
	gapStart := -1

	closeGap := func(endIdx int) {
		if gapStart >= 0 {
			if d.OnGap != nil {
				d.OnGap(DownloadGap{From: gapStart, To: endIdx})
			}
			gapStart = -1
		}
	}

	for r := range results {
		buffer[r.idx] = r.data

		// Flush consecutive entries (segments and gap sentinels) in order.
		for {
			data, ok := buffer[nextIdx]
			if !ok {
				break
			}
			delete(buffer, nextIdx) // Free memory immediately

			if data == nil {
				// Gap sentinel: skip without writing, coalescing runs of
				// missing segments. currentSeq MUST advance here (like the live
				// loop's gap-skip) so it stays in playlist-index space: the
				// resume filter in runHlsLoop is index-based
				// (segSeq = MediaSequence+i >= curSeq), so if currentSeq lagged
				// the index by the gap count, a resume would re-fetch and append
				// already-written segments — a duplicated/mis-ordered TS spliced
				// into the VOD.
				if gapStart < 0 {
					gapStart = nextIdx
				}
				d.currentSeq.Add(1)
				nextIdx++
				continue
			}
			closeGap(nextIdx - 1) // a real segment ends any open gap

			// fMP4: write this segment's governing init ahead of it (first
			// write, or an inline re-init at a mid-VOD map change — this path
			// never runs under StopOnGap, so ensureHlsInit never asks for a
			// part split here). Synchronous in the consumer: it fires once per
			// distinct init, not per segment.
			if initErr := d.ensureHlsInit(ctx, pl.Segments[nextIdx].MapURI); initErr != nil {
				return fmt.Errorf("HLS VOD init segment for %d: %w", nextIdx, initErr)
			}

			n, err := d.outputFile.Write(data)
			if err != nil {
				return fmt.Errorf("write HLS VOD segment %d: %w", nextIdx, err)
			}
			d.bytesWritten.Add(int64(n))
			// Seq reports the just-WRITTEN sequence — the same last-written
			// convention as every other emitter (see the live loop above) —
			// so the persisted seq columns mean one thing on every path.
			writtenSeq := int(d.currentSeq.Load())
			d.currentSeq.Add(1)
			nextIdx++

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   writtenSeq,
					Bytes: d.bytesWritten.Load(),
					Total: totalSegs,
				})
			}

			// Save resume state periodically — frequent enough that a crash
			// loses at most that many segments, rare enough that the
			// disk-touching saveResume isn't on the hot path for every segment.
			if d.currentSeq.Load()%50 == 0 {
				d.saveResume()
			}
		}
	}
	// Close a gap still open at consumer exit. nextIdx-1 is the last flushed
	// index — equal to totalSegs-1 after full consumption (every index emits a
	// result, so the flush loop drains to totalSegs), but the honest bound on
	// early exit via cancellation: drained workers emit nothing, and closing at
	// totalSegs-1 would record the entire unflushed remainder as a gap even
	// though those segments were never determined missing.
	closeGap(nextIdx - 1)

	// The whole VOD playlist has been consumed — natural end. Mark ended so
	// runHlsLoop's deferred ClearResume removes the sidecar (see the ENDLIST
	// path above).
	if !d.isCancelled() && ctx.Err() == nil {
		d.streamEnded.Store(true)
	}
	d.saveResume()
	return nil
}

# HLS live-loop reload pacing — port ffmpeg's discipline

## Problem

Moombox's live HLS loop (`internal/engine/downloader_hls.go` `runHlsLoop`, shared by
Twitch and YouTube HLS) fetches the media playlist, downloads all new segments
sequentially, then **unconditionally sleeps a fixed `TargetDuration`** (default 2s):

```go
// Wait before next refresh
targetDur := pl.TargetDuration
if targetDur <= 0 { targetDur = 2.0 }
utils.Sleep(ctx, time.Duration(targetDur*float64(time.Second)))
```

Because the sleep runs *after* the download work, the effective reload period is
`playlist_fetch + Σ(segment_downloads) + TargetDuration`, which is longer than the
rate at which the server publishes segments. Segments therefore accumulate into small
batches (~3 observed on Twitch) each refresh, and the recording trails the live edge.
No data loss — every segment is still fetched — but latency and bursty I/O.

## Goal

Replace the fixed post-work sleep with ffmpeg's reload discipline
(`libavformat/hls.c`) so the loop hugs the live edge and pulls ~one segment per
segment-interval. Applies to **all HLS** (Twitch + YouTube), the shared loop — no fork.
Keep Moombox's native pipeline (ad-skip, gapless parts, resume, offline recovery,
max-timeout backstop, gap detection). Timing-only change.

## ffmpeg reference (verbatim, master `libavformat/hls.c`)

```c
static int64_t default_reload_interval(struct playlist *pls) {
    return pls->n_segments > 0 ? pls->segments[pls->n_segments - 1]->duration
                               : pls->target_duration;
}
```
```c
// end of parse_playlist():
if (pls) pls->last_load_time = av_gettime_relative();
```
```c
reload:
    reload_count++;
    if (reload_count > c->max_reload) return AVERROR_EOF;
    if (!v->finished && av_gettime_relative() - v->last_load_time >= reload_interval) {
        if ((ret = parse_playlist(c, v->url, v, NULL)) < 0) { ...; return ret; }
        /* If we need to reload the playlist again below (if there's still no more
         * segments), switch to a reload interval of half the target duration. */
        reload_interval = v->target_duration / 2;
    }
    if (v->cur_seq_no >= v->start_seq_no + v->n_segments) {
        if (v->finished || v->is_subtitle) return AVERROR_EOF;
        while (av_gettime_relative() - v->last_load_time < reload_interval) {
            if (ff_check_interrupt(c->interrupt_callback)) return AVERROR_EXIT;
            av_usleep(100*1000);
        }
        goto reload;
    }
    // ... open segment cur_seq_no ...
// read_data_continuous(): after a segment finishes -> v->cur_seq_no++; goto restart;
```

Key facts confirmed against the source:
1. Reload interval = **last segment's duration** (fallback `target_duration`).
2. `last_load_time` is stamped at the **end of the playlist fetch/parse**; the reload
   gate is `now - last_load_time >= reload_interval`.
3. After a reload, the interval **halves to `target_duration/2`** for subsequent
   still-stalled reloads (the "expect changes soon" case).
4. Segments already in the playlist are downloaded **back-to-back with no wait**; the
   demuxer only waits (100 ms poll until `reload_interval` since `last_load_time`) when
   the segment list is **exhausted**. Already-past-interval ⇒ the `while` is false ⇒
   reload immediately.

## Design

### Pure helper (isolated, unit-tested)

```go
// hlsReloadDelay mirrors ffmpeg libavformat/hls.c reload pacing. It returns how
// long to wait before the next media-playlist reload, given the time already
// spent this cycle since the playlist was loaded.
//
//   - When the last refresh produced NEW segments (we're flowing), the interval is
//     the LAST segment's duration — ffmpeg default_reload_interval (fallback
//     targetDur, then 2s if the playlist declared neither).
//   - When it produced NO new segments (stalled at the live edge), the interval is
//     targetDur/2 — ffmpeg's post-reload "still no segments" halving.
//   - The result is the REMAINDER of that interval after `elapsed` (time since the
//     playlist was fetched), clamped to 0 so we reload immediately when already
//     behind — matching ffmpeg's `now - last_load_time >= reload_interval`.
func hlsReloadDelay(lastSegDur, targetDur float64, hadNewSegments bool, elapsed time.Duration) time.Duration
```

Fidelity note (documented, not a defect): ffmpeg toggles `reload_interval` to
`target/2` *within a single read_data call* after its first reload; Moombox's
one-fetch-per-iteration loop can't hold that statement literally. The behavior-
equivalent mapping is "flowing ⇒ last-seg duration, stalled ⇒ target/2," which
produces the same observable cadence (each cycle that consumes a segment waits ~one
segment-duration; the first cycle that finds nothing drops to half-target polling).

### Loop wiring (`runHlsLoop`)

1. Stamp `loadTime := time.Now()` immediately **after** the media playlist is fetched
   and parsed each iteration (ffmpeg's `last_load_time`).
2. Track `lastSegDur` = duration of the final segment in the parsed playlist
   (`pl.Segments[len-1].Duration`), and `hadNewSegments` = `len(newSegments) > 0`.
3. Replace the tail sleep with:
   ```go
   utils.Sleep(ctx, hlsReloadDelay(lastSegDur, pl.TargetDuration, len(newSegments) > 0, time.Since(loadTime)))
   ```
   `utils.Sleep` is already `ctx`-cancellable, so a single call is the behavioral
   equivalent of ffmpeg's 100 ms interrupt-checked poll — no busy-poll needed.

### Preserved unchanged
Stitched-ad skipping, gapless part-splitting, resume-sidecar save cadence, offline
pause/resume (`waitOnline`), the `EnforceMaxTimeout` backstop, gap detection, the
stale-window → `CheckStreamStatus` end verification (Moombox's superior analog of
ffmpeg's `max_reload`/`m3u8_hold_counters` give-up guard), and the VOD parallel path
(`runHlsVodParallel`). The per-iteration offline/backstop checks still run once per
reload at ~segment cadence. Reload rate stays floored by the interval, so no extra
server load.

## Testing

- Unit-test `hlsReloadDelay` (pure): flowing ⇒ last-seg basis; stalled ⇒ target/2;
  `lastSegDur<=0` ⇒ targetDur fallback; `targetDur<=0` ⇒ 2s fallback; `elapsed`
  exceeds interval ⇒ 0 (immediate reload); partial `elapsed` ⇒ correct remainder.
  Each case's expected value is derived directly from the ffmpeg formulas above.
- Existing HLS tests (`downloader_hls_maxtimeout_test.go`, etc.) stay green.
- Optional temporary debug log during a live Twitch recording to confirm the batch
  collapses to ~1.

## Risk
Low — timing only. Correctness (every segment downloaded) and all edge-case handling
unchanged. Watch-item: don't reload-hammer when perpetually behind — mitigated because
the interval floors the caught-up cadence and being behind is transient (and the
backstop/stale logic still ends dead streams).

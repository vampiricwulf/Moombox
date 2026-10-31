package youtube

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The pre-roll ad wait, yt-dlp's available_at.
//
// A player response for an account that is shown ads carries the pre-roll ad
// the player must play before the video, and YouTube refuses the media URLs
// of that response until the ad would have ended — or, for a skippable ad,
// until it could have been skipped. A signed-in non-Premium account's
// whole-file VOD was refused at its very first request (the size probe), and
// so was every fresh URL fetched seconds later, each from a fresh response
// with an ad of its own, where the same account's yt-dlp waited and
// downloaded the video; a Premium account never gets the ad, and never
// noticed.
//
// yt-dlp reads the wait off each player response (_get_available_at_timestamp,
// 715af0c63 / #14646; the adPlacements half is 3c279b33c / #17062), stamps it
// on every format that response yields as available_at, and its downloader
// sleeps until the latest available_at of the formats it fetches before the
// first request ("Sleeping N seconds as required by the site",
// downloader/common.py). Before #14646 it slept a fixed 6 s after every
// player response. This file is the extraction half, stamped by
// parsePlayerResponse on every response the cascades parse (each client's and
// the watch page's ytInitialPlayerResponse); the worker's waitForPlayback is
// the downloader's.

// PlaybackWaitApplies reports whether a download of a stream in this status
// waits for its formats' AvailableAt. Not one that is live or post-live:
// upstream stamps available_at only `if live_status not in ('is_live',
// 'post_live')` (_video.py, process_format_stream and
// process_manifest_format), so neither its live capture nor its post-live
// download ever sleeps on a pre-roll ad. Everything else waits — a finished
// broadcast, a plain upload. The status is the extraction's, as upstream's
// live_status is one verdict over all its player responses, which is why the
// stamp itself is made on every response and the rule is applied here.
func PlaybackWaitApplies(status StreamStatus) bool {
	return status != StreamLive && status != StreamPostLive
}

// playbackNow is the clock a player response's pre-roll wait is measured
// from.
func (p *PlayerAPI) playbackNow() time.Time {
	if p.playbackClock != nil {
		return p.playbackClock()
	}
	return time.Now()
}

// playbackAvailableAt is upstream's _get_available_at_timestamp: the instant
// the media of the player response in data may first be requested, or the
// zero time when the response carries no pre-roll ad (upstream's int(now),
// which is already past by the time anything downloads). The waits of the
// response's pre-roll ads add up, each the time until that ad may be skipped
// when it is skippable, else its length; an ad that states neither adds
// nothing. The instant is the next whole second after now plus that sum, as
// upstream's math.ceil(now) + wait_seconds. Each ad that counts is logged at
// Debug, as upstream's write_debug, naming the client the response came from.
func (p *PlayerAPI) playbackAvailableAt(ctx context.Context, data map[string]any, videoID string) time.Time {
	var wait float64
	for _, r := range preRollAdRenderers(data) {
		secs, desc, ok := preRollAdWait(r)
		if !ok {
			continue
		}
		p.logger.Debug("[PlayerApi] pre-roll ad detected",
			"videoID", videoID, "client", playerCallScopeFrom(ctx).clientName(), "ad", desc)
		wait += secs
	}
	if wait == 0 {
		return time.Time{}
	}
	now := p.playbackNow()
	ceil := now.Truncate(time.Second)
	if ceil.Before(now) {
		ceil = ceil.Add(time.Second)
	}
	return ceil.Add(time.Duration(wait * float64(time.Second)))
}

// preRollAdRenderers returns the instreamVideoAdRenderer of every ad the
// response plays BEFORE the video, in upstream's traversal order: the
// adPlacements whose adPlacementConfig.kind is AD_PLACEMENT_KIND_START, then
// the adSlots whose triggerEvent is SLOT_TRIGGER_EVENT_BEFORE_CONTENT — a
// slot's renderingContent holds the renderer itself, or a sequence of layouts
// that each hold one. A mid-roll or post-roll placement, and any slot
// triggered later, is not waited for.
func preRollAdRenderers(data map[string]any) []map[string]any {
	var out []map[string]any
	add := func(content map[string]any) {
		if r, ok := content["instreamVideoAdRenderer"].(map[string]any); ok {
			out = append(out, r)
		}
	}
	placements, _ := data["adPlacements"].([]any)
	for _, item := range placements {
		v, _ := item.(map[string]any)
		if getDeepStr(v, "adPlacementRenderer", "config", "adPlacementConfig", "kind") != "AD_PLACEMENT_KIND_START" {
			continue
		}
		if r, ok := getNestedMap(v, "adPlacementRenderer", "renderer"); ok {
			add(r)
		}
	}
	slots, _ := data["adSlots"].([]any)
	for _, item := range slots {
		v, _ := item.(map[string]any)
		if getDeepStr(v, "adSlotRenderer", "adSlotMetadata", "triggerEvent") != "SLOT_TRIGGER_EVENT_BEFORE_CONTENT" {
			continue
		}
		content, ok := getNestedMap(v, "adSlotRenderer", "fulfillmentContent", "fulfilledLayout", "playerBytesAdLayoutRenderer", "renderingContent")
		if !ok {
			continue
		}
		add(content)
		layouts, _ := getNestedSlice(content, "playerBytesSequentialLayoutRenderer", "sequentialLayouts")
		for _, l := range layouts {
			lm, _ := l.(map[string]any)
			if c, ok := getNestedMap(lm, "playerBytesAdLayoutRenderer", "renderingContent"); ok {
				add(c)
			}
		}
	}
	return out
}

// preRollAdWait is how long one pre-roll ad holds the video back, in seconds,
// and upstream's words for it ("a 15s ad skippable after 5s"): the time until
// it may be skipped (skipOffsetMilliseconds) when it is skippable, else its
// length — the last length_seconds in its playerVars query string. ok is
// false for an ad that states neither, which upstream neither counts nor
// logs.
func preRollAdWait(renderer map[string]any) (secs float64, desc string, ok bool) {
	length, haveLength := adLengthSeconds(renderer)
	desc = "an ad"
	if haveLength {
		secs, ok = float64(length), true
		desc = "a " + strconv.Itoa(length) + "s ad"
	}
	if skip, haveSkip := adSkipSeconds(renderer); haveSkip {
		secs, ok = skip, true
		desc += " skippable after " + strconv.FormatFloat(skip, 'f', -1, 64) + "s"
	}
	return secs, desc, ok
}

// adLengthSeconds reads an ad's length as upstream does — playerVars through
// parse_qs, the LAST length_seconds value, int_or_none — and false when there
// is none to read. parse_qs drops blank values, so the last non-blank one is
// the one taken, and int() ignores surrounding whitespace.
func adLengthSeconds(renderer map[string]any) (int, bool) {
	pv, ok := renderer["playerVars"].(string)
	if !ok {
		return 0, false
	}
	q, _ := url.ParseQuery(pv) // keeps every pair that did parse, as parse_qs does
	vals := q["length_seconds"]
	for i := len(vals) - 1; i >= 0; i-- {
		if vals[i] == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(vals[i]))
		return n, err == nil
	}
	return 0, false
}

// adSkipSeconds reads skipOffsetMilliseconds as upstream's
// float_or_none(scale=1000): a number, or a string holding one, in seconds.
func adSkipSeconds(renderer map[string]any) (float64, bool) {
	switch v := renderer["skipOffsetMilliseconds"].(type) {
	case float64:
		return v / 1000, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f / 1000, true
		}
	}
	return 0, false
}

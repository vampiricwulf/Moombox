package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// globalEmoteRetryFloor is how long a provider's global set is left alone
// after a fetch of it failed. Resolve runs at the end of every chat download
// and every part roll; without the floor a provider that is down would cost
// each of them a request and up to emoteTimeout of waiting.
const globalEmoteRetryFloor = 5 * time.Minute

// The three providers' global sets, in the order the resolver keeps them.
const (
	globalBTTV = iota
	globalFFZ
	globalSevenTV
	globalProviderCount
)

var globalProviderNames = [globalProviderCount]string{"bttv", "ffz", "7tv"}

// globalEmoteSet is one provider's global emote set: the emotes every Twitch
// channel gets from that provider. One per provider per process, because the
// set does not depend on the channel.
type globalEmoteSet struct {
	emotes    []EmoteInfo
	fetchedAt time.Time // zero until the provider has answered once
	triedAt   time.Time // the last fetch attempt, answered or not
}

// due reports whether the set should be fetched now: never answered or older
// than emoteCacheTTL, and not inside the retry floor of a failed attempt.
func (g *globalEmoteSet) due(now time.Time) bool {
	if !g.fetchedAt.IsZero() && now.Sub(g.fetchedAt) < emoteCacheTTL {
		return false
	}
	lastTryFailed := g.triedAt.After(g.fetchedAt)
	return !lastTryFailed || now.Sub(g.triedAt) >= globalEmoteRetryFloor
}

// globalEmotes returns the three providers' global emote sets, fetching the
// ones that are due. The rules are the per-channel cache's (Resolve): a set
// is kept for emoteCacheTTL, a failed fetch is not an answer and is not
// cached, and a failed refresh keeps serving the set it had. Each provider
// stands alone, so one being down costs the other two nothing.
//
// It never fails: a provider that has never answered contributes no emotes.
// Concurrent callers share one fetch; a caller whose ctx ends while waiting
// gets what is cached. The fetch runs on the first caller's ctx: when that
// ctx ends before a provider answers, the attempt is not counted as a failure
// of the provider (no retry floor, no warning), and the callers that waited
// on it fetch for themselves.
func (er *EmoteResolver) globalEmotes(ctx context.Context) *TwitchEmoteData {
	er.mu.Lock()
	for er.globalInflight != nil {
		wait := er.globalInflight
		er.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			er.mu.Lock()
			defer er.mu.Unlock()
			return er.globalSnapshotLocked()
		}
		er.mu.Lock()
	}
	now := er.now()
	var due [globalProviderCount]bool
	var prevTried [globalProviderCount]time.Time
	anyDue := false
	for i := range er.globals {
		if er.globals[i].due(now) {
			due[i] = true
			anyDue = true
			prevTried[i] = er.globals[i].triedAt
			er.globals[i].triedAt = now
		}
	}
	if !anyDue {
		defer er.mu.Unlock()
		return er.globalSnapshotLocked()
	}
	done := make(chan struct{})
	er.globalInflight = done
	er.mu.Unlock()

	fetchers := [globalProviderCount]func(context.Context) ([]EmoteInfo, bool){
		globalBTTV:    er.fetchBTTVGlobal,
		globalFFZ:     er.fetchFFZGlobal,
		globalSevenTV: er.fetch7TVGlobal,
	}
	var results [globalProviderCount][]EmoteInfo
	var answered [globalProviderCount]bool
	var wg sync.WaitGroup
	for i := range fetchers {
		if !due[i] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					er.logger.Error("global emote fetch panic", "provider", globalProviderNames[i], "panic", r)
				}
			}()
			results[i], answered[i] = fetchers[i](ctx)
		}()
	}
	wg.Wait()

	er.mu.Lock()
	stamp := er.now()
	// A fetch that ended with the caller's own ctx cancelled or expired says
	// nothing about the provider: the attempt is taken back, so the retry
	// floor does not start and the next caller (a waiter on this very fetch
	// included) fetches at once.
	callerGone := ctx.Err() != nil
	for i := range er.globals {
		switch {
		case answered[i]:
			er.globals[i].emotes = results[i]
			er.globals[i].fetchedAt = stamp
		case due[i] && callerGone:
			er.globals[i].triedAt = prevTried[i]
		}
	}
	snapshot := er.globalSnapshotLocked()
	er.globalInflight = nil
	er.mu.Unlock()
	close(done)
	return snapshot
}

// globalSnapshotLocked is the cached global sets as one TwitchEmoteData.
// Caller holds er.mu. The slices are shared with the cache and never written
// to after they are stored.
func (er *EmoteResolver) globalSnapshotLocked() *TwitchEmoteData {
	return &TwitchEmoteData{
		BTTV:    er.globals[globalBTTV].emotes,
		FFZ:     er.globals[globalFFZ].emotes,
		SevenTV: er.globals[globalSevenTV].emotes,
	}
}

// withGlobalEmotes returns channel's emotes followed by the global ones, each
// provider's globals appended to that provider's own list. A global emote
// whose code any of the channel's three lists already has is left out: a
// channel emote wins over a global one of the same name, whichever provider
// either came from, as in chatterino7 (MessageBuilder.cpp checks FFZ, BTTV
// and 7TV channel emotes before any global set). Dropping the loser here,
// rather than leaving it to the reader, is what makes that hold in a player
// that merges the three lists one after another.
//
// channel is not modified (it is the cached entry); the result is a new value
// with new slices wherever anything was appended.
func withGlobalEmotes(channel, global *TwitchEmoteData) *TwitchEmoteData {
	if channel == nil || global == nil ||
		len(global.BTTV)+len(global.FFZ)+len(global.SevenTV) == 0 {
		return channel
	}
	taken := make(map[string]struct{}, len(channel.BTTV)+len(channel.FFZ)+len(channel.SevenTV))
	for _, list := range [][]EmoteInfo{channel.BTTV, channel.FFZ, channel.SevenTV} {
		for _, e := range list {
			taken[e.Code] = struct{}{}
		}
	}
	merge := func(own, globals []EmoteInfo) []EmoteInfo {
		out := make([]EmoteInfo, len(own), len(own)+len(globals))
		copy(out, own)
		for _, e := range globals {
			if _, ok := taken[e.Code]; !ok {
				out = append(out, e)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return &TwitchEmoteData{
		BTTV:    merge(channel.BTTV, global.BTTV),
		FFZ:     merge(channel.FFZ, global.FFZ),
		SevenTV: merge(channel.SevenTV, global.SevenTV),
	}
}

// fetchGlobalJSON fetches one global-set endpoint. Unlike a per-channel
// lookup, a 404 here is a failure like any other: the global sets exist.
func (er *EmoteResolver) fetchGlobalJSON(ctx context.Context, provider, url string) ([]byte, bool) {
	fetchCtx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()
	data, err := fetchJSON(fetchCtx, url)
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up; the provider did not fail.
			return nil, false
		}
		er.logger.Warn("global emote set unavailable", "provider", provider, "err", err)
		return nil, false
	}
	return data, true
}

// fetchBTTVGlobal returns BTTV's global emotes and whether BTTV answered.
func (er *EmoteResolver) fetchBTTVGlobal(ctx context.Context) ([]EmoteInfo, bool) {
	data, ok := er.fetchGlobalJSON(ctx, "bttv", constants.TwitchEmoteAPIs.BTTVGlobal)
	if !ok {
		return nil, false
	}
	var resp []struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("global emote set unreadable", "provider", "bttv", "err", err)
		return nil, false
	}
	emotes := make([]EmoteInfo, 0, len(resp))
	for _, e := range resp {
		emotes = append(emotes, EmoteInfo{ID: e.ID, Code: e.Code, URL: fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x.webp", e.ID)})
	}
	return emotes, true
}

// fetchFFZGlobal returns FFZ's global emotes and whether FFZ answered. Only
// the sets the response names in default_sets count, as in chatterino7
// (FfzEmotes.cpp parseGlobalEmotes): the endpoint also lists sets that are
// not on for everyone.
func (er *EmoteResolver) fetchFFZGlobal(ctx context.Context) ([]EmoteInfo, bool) {
	data, ok := er.fetchGlobalJSON(ctx, "ffz", constants.TwitchEmoteAPIs.FFZGlobal)
	if !ok {
		return nil, false
	}
	var resp struct {
		DefaultSets []int `json:"default_sets"`
		Sets        map[string]struct {
			ID        int           `json:"id"`
			Emoticons []ffzEmoticon `json:"emoticons"`
		} `json:"sets"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("global emote set unreadable", "provider", "ffz", "err", err)
		return nil, false
	}
	var emotes []EmoteInfo
	for _, set := range resp.Sets {
		isDefault := false
		for _, id := range resp.DefaultSets {
			if id == set.ID {
				isDefault = true
				break
			}
		}
		if isDefault {
			emotes = appendFFZEmotes(emotes, set.Emoticons)
		}
	}
	return emotes, true
}

// fetch7TVGlobal returns 7TV's global emotes and whether 7TV answered.
func (er *EmoteResolver) fetch7TVGlobal(ctx context.Context) ([]EmoteInfo, bool) {
	data, ok := er.fetchGlobalJSON(ctx, "7tv", constants.TwitchEmoteAPIs.SevenTVGlobal)
	if !ok {
		return nil, false
	}
	var resp struct {
		Emotes []sevenTVEmote `json:"emotes"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("global emote set unreadable", "provider", "7tv", "err", err)
		return nil, false
	}
	return sevenTVEmoteInfos(resp.Emotes), true
}

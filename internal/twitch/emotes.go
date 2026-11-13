package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

const emoteTimeout = 8 * time.Second

// emoteCacheTTL bounds how long a channel's third-party emote set is served
// from cache. Moombox runs for weeks at a time and 7TV/BTTV/FFZ sets change
// daily, so a cache with no expiry archives a channel's chat against the emote
// set it had the first time the daemon saw it.
//
// A day is long enough that the three APIs are hit once per channel per day
// even for a channel that is live every day, and short enough that a new emote
// shows up in the next archive rather than the next restart.
const emoteCacheTTL = 24 * time.Hour

// emoteCacheEntry is one channel's resolved set plus WHEN it was resolved.
// The timestamp is the whole reason this is a struct rather than the bare
// pointer it used to be.
//
// retryAfter is set when the fetch that wrote the entry left a provider
// unanswered: the entry is then fresh only until that time (see
// resolveChannel), not for emoteCacheTTL. Zero when all three answered.
type emoteCacheEntry struct {
	data       *TwitchEmoteData
	fetchedAt  time.Time
	retryAfter time.Time
}

// fresh reports whether the entry is served without a fetch.
func (e emoteCacheEntry) fresh(now time.Time) bool {
	if !e.retryAfter.IsZero() {
		return now.Before(e.retryAfter)
	}
	return now.Sub(e.fetchedAt) < emoteCacheTTL
}

// EmoteResolver fetches and caches third-party emotes for Twitch channels.
type EmoteResolver struct {
	mu         sync.Mutex
	cache      map[string]emoteCacheEntry // channelLogin (lowered) -> entry
	cacheOrder []string                   // insertion order for LRU eviction
	inflight   map[string]chan struct{}   // dedup concurrent fetches for same key

	// globals are the providers' global emote sets (emotes_global.go), one per
	// provider for the whole process; globalInflight is non-nil while a fetch
	// of them runs. Both are guarded by mu.
	globals        [globalProviderCount]globalEmoteSet
	globalInflight chan struct{}

	// now is the clock the TTL is measured against. A field so a test can
	// cross a 24-hour boundary without waiting for one; production never
	// assigns it.
	now func() time.Time

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewEmoteResolver creates a new emote resolver.
func NewEmoteResolver(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *EmoteResolver {
	return &EmoteResolver{
		cache:    make(map[string]emoteCacheEntry),
		inflight: make(map[string]chan struct{}),
		now:      time.Now,
		logger:   logger,
	}
}

// evictIfFullLocked drops the oldest entry when the cache is at its 200-channel
// ceiling. Caller holds er.mu.
func (er *EmoteResolver) evictIfFullLocked() {
	const maxEmoteCacheEntries = 200
	if len(er.cache) < maxEmoteCacheEntries || len(er.cacheOrder) == 0 {
		return
	}
	oldest := er.cacheOrder[0]
	er.cacheOrder = er.cacheOrder[1:]
	delete(er.cache, oldest)
}

// Resolve returns the third-party emotes a channel's chat can use: the
// channel's own BTTV, FFZ and 7TV sets (resolveChannel) followed by the three
// providers' global sets (globalEmotes), with a channel emote winning over a
// global one of the same name (withGlobalEmotes). The two are fetched side by
// side, so a cold start costs one emoteTimeout at most, not two.
//
// The global sets only ever add: when they cannot be fetched the channel's
// sets are returned alone, and when the channel's sets cannot be resolved the
// result is nil whatever the globals hold, so nothing is written to the chat file
// and the next resolve (the job's next part, resolveEmotes in
// chat_recording.go) tries again.
func (er *EmoteResolver) Resolve(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
	var global *TwitchEmoteData
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("global emote resolve panic", "panic", r)
			}
		}()
		global = er.globalEmotes(ctx)
	}()
	channel := er.resolveChannel(ctx, channelID, channelLogin...)
	<-done
	return withGlobalEmotes(channel, global)
}

// resolveChannel fetches a channel's own third-party emotes.
//
// Results are cached per channel login (lowercased); the channelID is used for
// the API calls. Two rules beyond the plain LRU, and both exist because this
// process runs for weeks (T1-11):
//
//   - A FAILURE IS NOT A RESULT. The set is cached only when at least one of
//     the three providers answered — including answering with no emotes, which
//     is the honest state of many channels. Three providers failing together
//     (one outage, one flaky minute) used to be written to the cache and served
//     for the rest of the process lifetime.
//   - AN ANSWER GOES STALE. An entry older than emoteCacheTTL is refetched on
//     the next Resolve. If that refetch fails outright the STALE set is served
//     and kept: an emote set from yesterday beats none.
//   - ONE PROVIDER FAILING IS NOT "NO EMOTES" EITHER. When some providers
//     answer and others fail, the answers are cached, each failed provider
//     keeps the list the entry already had for it (none on a first fetch),
//     and the entry is refetched after globalEmoteRetryFloor rather than after
//     emoteCacheTTL. Without this a provider's bad minute at one stream's end
//     left the channel's archives without that provider's emotes for a day,
//     and a refresh replaced a complete set with the poorer one. A fetch that
//     ended because the caller's own ctx did says nothing about a provider: it
//     starts no floor, so the next Resolve fetches at once (the global sets'
//     rule, globalEmotes).
//
// Returns nil only when nothing is cached and no provider answered, so the
// caller (resolveEmotes, chat_recording.go) can retry later.
func (er *EmoteResolver) resolveChannel(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
	// Determine cache key: prefer channelLogin, fall back to channelID
	cacheKey := channelID
	if len(channelLogin) > 0 && channelLogin[0] != "" {
		cacheKey = strings.ToLower(channelLogin[0])
	}

	er.mu.Lock()
	var stale *TwitchEmoteData
	if cached, ok := er.cache[cacheKey]; ok {
		// LRU: move to end of order list
		for i, k := range er.cacheOrder {
			if k == cacheKey {
				er.cacheOrder = append(er.cacheOrder[:i], er.cacheOrder[i+1:]...)
				er.cacheOrder = append(er.cacheOrder, cacheKey)
				break
			}
		}
		if cached.fresh(er.now()) {
			er.mu.Unlock()
			return cached.data
		}
		// Expired, or due a retry for a provider that failed: keep it in hand
		// as the fallback for a refetch that fails, and leave it in the map so
		// a concurrent caller keeps being served.
		stale = cached.data
	}
	// Dedup: if another goroutine is already fetching this key, wait for it.
	// Respect ctx so a cancelled download doesn't sit here waiting on a
	// fetcher that may itself be blocked on network IO.
	if wait, ok := er.inflight[cacheKey]; ok {
		er.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return stale
		}
		// Now it should be in cache (unless ctx cancelled and fetcher hadn't
		// populated yet — in which case cache miss is fine, Twitch emote
		// resolution is best-effort).
		er.mu.Lock()
		cached, ok := er.cache[cacheKey]
		er.mu.Unlock()
		if ok {
			return cached.data
		}
		return stale
	}
	// Mark this key as inflight
	done := make(chan struct{})
	er.inflight[cacheKey] = done
	er.mu.Unlock()

	er.logger.Debug("resolving emotes", "channelID", channelID)

	// Fetch all providers in parallel
	var wg sync.WaitGroup
	var bttvResult, ffzResult, sevenTVResult []EmoteInfo
	var bttvOK, ffzOK, sevenTVOK bool

	wg.Add(3)

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("BTTV emote fetch panic", "panic", r)
			}
		}()
		bttvResult, bttvOK = er.fetchBTTV(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("FFZ emote fetch panic", "panic", r)
			}
		}()
		ffzResult, ffzOK = er.fetchFFZ(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("7TV emote fetch panic", "panic", r)
			}
		}()
		sevenTVResult, sevenTVOK = er.fetch7TV(ctx, channelID)
	}()

	wg.Wait()

	answered := bttvOK || ffzOK || sevenTVOK
	complete := bttvOK && ffzOK && sevenTVOK
	// A provider that failed keeps the list the entry had for it. stale is
	// never written to after it is stored, so its slices can be shared.
	if stale != nil {
		if !bttvOK {
			bttvResult = stale.BTTV
		}
		if !ffzOK {
			ffzResult = stale.FFZ
		}
		if !sevenTVOK {
			sevenTVResult = stale.SevenTV
		}
	}
	data := &TwitchEmoteData{
		BTTV:    bttvResult,
		FFZ:     ffzResult,
		SevenTV: sevenTVResult,
	}

	er.mu.Lock()
	if answered {
		entry := emoteCacheEntry{data: data, fetchedAt: er.now()}
		if !complete {
			// Due again after the floor, or at once when the failures were
			// the caller's ctx ending rather than the providers'.
			entry.retryAfter = entry.fetchedAt
			if ctx.Err() == nil {
				entry.retryAfter = entry.fetchedAt.Add(globalEmoteRetryFloor)
			}
		}
		// Refreshing an EXPIRED entry must not evict anything and must not
		// append a second order entry: the key is already in both, and the LRU
		// promotion above already moved it to the end.
		if _, existing := er.cache[cacheKey]; !existing {
			er.evictIfFullLocked()
			er.cacheOrder = append(er.cacheOrder, cacheKey)
		}
		er.cache[cacheKey] = entry
	}
	delete(er.inflight, cacheKey)
	er.mu.Unlock()
	// Release er.mu before close(done) so any waiting goroutines wake up
	// and re-acquire er.mu cleanly without contending against the still-
	// held lock. Minor throughput win under high concurrent resolve rates.
	close(done)

	if !answered {
		if stale != nil {
			er.logger.Warn("emote refresh failed; serving the cached set",
				"channelID", channelID)
			return stale
		}
		er.logger.Warn("every third-party emote provider failed; not caching",
			"channelID", channelID)
		return nil
	}

	total := len(bttvResult) + len(ffzResult) + len(sevenTVResult)
	if total > 0 {
		er.logger.Debug("emotes resolved",
			"channelID", channelID,
			"bttv", len(bttvResult),
			"ffz", len(ffzResult),
			"7tv", len(sevenTVResult))
	}

	return data
}

// Clear empties the emote cache (e.g., on shutdown).
func (er *EmoteResolver) Clear() {
	er.mu.Lock()
	er.cache = make(map[string]emoteCacheEntry)
	er.cacheOrder = nil
	er.globals = [globalProviderCount]globalEmoteSet{}
	er.mu.Unlock()
}

// fetchBTTV returns the channel's BTTV emotes and whether BTTV ANSWERED.
// The bool is not "found emotes": a channel with no BTTV emotes is a real,
// cacheable answer, and only a provider that could not be reached or read is a
// failure (see Resolve).
func (er *EmoteResolver) fetchBTTV(ctx context.Context, channelID string) ([]EmoteInfo, bool) {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.BTTVChannel, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		if errors.Is(err, errEmoteProviderNotFound) {
			// A real answer: this channel has no BTTV emotes. Cacheable, and
			// deliberately not a Warn — see errEmoteProviderNotFound.
			er.logger.Debug("bttv has no record of this channel", "channelID", channelID)
			return nil, true
		}
		// Warn rather than Debug — a persistently-down emote provider was
		// invisible at the default log level, so missing emotes looked like
		// a Moombox bug. Audit-finding twitch.md #36.
		er.logger.Warn("bttv fetch failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var resp struct {
		ChannelEmotes []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"channelEmotes"`
		SharedEmotes []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"sharedEmotes"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("bttv parse failed", "err", err, "channelID", channelID)
		return nil, false
	}

	emotes := make([]EmoteInfo, 0, len(resp.ChannelEmotes)+len(resp.SharedEmotes))
	for _, e := range resp.ChannelEmotes {
		emotes = append(emotes, EmoteInfo{
			ID:   e.ID,
			Code: e.Code,
			URL:  fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x.webp", e.ID),
		})
	}
	for _, e := range resp.SharedEmotes {
		emotes = append(emotes, EmoteInfo{
			ID:   e.ID,
			Code: e.Code,
			URL:  fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x.webp", e.ID),
		})
	}

	return emotes, true
}

// fetchFFZ returns the channel's FFZ emotes and whether FFZ ANSWERED.
// The bool is not "found emotes": a channel with no FFZ emotes is a real,
// cacheable answer, and only a provider that could not be reached or read is a
// failure (see Resolve).
func (er *EmoteResolver) fetchFFZ(ctx context.Context, channelID string) ([]EmoteInfo, bool) {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.FFZChannel, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		if errors.Is(err, errEmoteProviderNotFound) {
			// A real answer: this channel has no FFZ emotes. Cacheable, and
			// deliberately not a Warn — see errEmoteProviderNotFound.
			er.logger.Debug("ffz has no record of this channel", "channelID", channelID)
			return nil, true
		}
		// Audit-finding twitch.md #36 — see fetchBTTV.
		er.logger.Warn("ffz fetch failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var resp struct {
		Sets map[string]struct {
			Emoticons []ffzEmoticon `json:"emoticons"`
		} `json:"sets"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("ffz parse failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var emotes []EmoteInfo
	for _, set := range resp.Sets {
		emotes = appendFFZEmotes(emotes, set.Emoticons)
	}

	return emotes, true
}

// ffzEmoticon is one emote of an FFZ set, as both the room and the global
// endpoint list it.
type ffzEmoticon struct {
	ID   int               `json:"id"`
	Name string            `json:"name"`
	URLs map[string]string `json:"urls"`
}

// appendFFZEmotes appends list to emotes as EmoteInfo: the 2x image, else the
// 1x, and an emoticon with neither is skipped.
func appendFFZEmotes(emotes []EmoteInfo, list []ffzEmoticon) []EmoteInfo {
	for _, e := range list {
		emoteURL := ""
		if u, ok := e.URLs["2"]; ok {
			emoteURL = u
		} else if u, ok := e.URLs["1"]; ok {
			emoteURL = u
		}
		if emoteURL != "" {
			// Fix protocol-relative URLs
			if strings.HasPrefix(emoteURL, "//") {
				emoteURL = "https:" + emoteURL
			}
			emotes = append(emotes, EmoteInfo{
				ID:   fmt.Sprintf("%d", e.ID),
				Code: e.Name,
				URL:  emoteURL,
			})
		}
	}
	return emotes
}

// fetch7TV returns the channel's 7TV emotes and whether 7TV ANSWERED.
// The bool is not "found emotes": a channel with no 7TV emotes is a real,
// cacheable answer, and only a provider that could not be reached or read is a
// failure (see Resolve).
func (er *EmoteResolver) fetch7TV(ctx context.Context, channelID string) ([]EmoteInfo, bool) {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.SevenTVUser, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		if errors.Is(err, errEmoteProviderNotFound) {
			// A real answer: this channel has no 7TV emotes. Cacheable, and
			// deliberately not a Warn — see errEmoteProviderNotFound.
			er.logger.Debug("7tv has no record of this channel", "channelID", channelID)
			return nil, true
		}
		// Audit-finding twitch.md #36 — see fetchBTTV.
		er.logger.Warn("7tv fetch failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var resp struct {
		EmoteSet struct {
			Emotes []sevenTVEmote `json:"emotes"`
		} `json:"emote_set"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("7tv parse failed", "err", err, "channelID", channelID)
		return nil, false
	}

	return sevenTVEmoteInfos(resp.EmoteSet.Emotes), true
}

// sevenTVEmote is one entry of a 7TV emote set, as both the per-channel and
// the global endpoint list it.
type sevenTVEmote struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Data struct {
		Host struct {
			URL   string `json:"url"`
			Files []struct {
				Name   string `json:"name"`
				Format string `json:"format"`
			} `json:"files"`
		} `json:"host"`
	} `json:"data"`
}

// sevenTVEmoteInfos turns a 7TV emote list into EmoteInfo, skipping an entry
// with no host URL or no file.
func sevenTVEmoteInfos(list []sevenTVEmote) []EmoteInfo {
	var emotes []EmoteInfo
	for _, e := range list {
		baseURL := e.Data.Host.URL
		if baseURL == "" {
			continue
		}

		// Prefer 2x webp, fallback to 1x
		fileName := ""
		for _, f := range e.Data.Host.Files {
			if f.Name == "2x.webp" {
				fileName = f.Name
				break
			}
		}
		if fileName == "" {
			for _, f := range e.Data.Host.Files {
				if f.Name == "1x.webp" {
					fileName = f.Name
					break
				}
			}
		}
		if fileName == "" && len(e.Data.Host.Files) > 0 {
			fileName = e.Data.Host.Files[0].Name
		}

		if fileName != "" {
			// 7TV host.url is typically protocol-relative (//cdn.7tv.app/...)
			hostURL := baseURL
			if strings.HasPrefix(hostURL, "//") {
				hostURL = "https:" + hostURL
			}
			emoteURL := hostURL + "/" + fileName
			emotes = append(emotes, EmoteInfo{
				ID:   e.ID,
				Code: e.Name,
				URL:  emoteURL,
			})
		}
	}

	return emotes
}

// errEmoteProviderNotFound marks a 404 from a third-party emote provider.
// It is an ANSWER, not a failure: BTTV, FFZ and 7TV all answer 404 for a
// channel that never registered with them, which is the honest state of many
// channels (measured 2026-09-15: id 141981764 answers 404 on all three).
// Reading it as a failure meant nothing was cached, so Resolve re-fired three
// requests and four Warn lines on every part roll and stream end of every job
// on that channel, for the life of the daemon (TWITCH-4).
var errEmoteProviderNotFound = errors.New("emote provider has no record of this channel")

func fetchJSON(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", constants.UserAgents.Web)

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Drain a bounded prefix so the connection is reusable, then report
		// the sentinel — the caller turns it into an empty, cacheable answer.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, errEmoteProviderNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
}

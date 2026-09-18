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
type emoteCacheEntry struct {
	data      *TwitchEmoteData
	fetchedAt time.Time
}

// EmoteResolver fetches and caches third-party emotes for Twitch channels.
type EmoteResolver struct {
	mu         sync.Mutex
	cache      map[string]emoteCacheEntry // channelLogin (lowered) -> entry
	cacheOrder []string                   // insertion order for LRU eviction
	inflight   map[string]chan struct{}   // dedup concurrent fetches for same key

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

// Resolve fetches all third-party emotes for a channel.
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
//
// Returns nil only when nothing is cached and no provider answered, so the
// caller's own cache (resolveEmotesCached, chat_recording.go) can retry later.
func (er *EmoteResolver) Resolve(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
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
		if er.now().Sub(cached.fetchedAt) < emoteCacheTTL {
			er.mu.Unlock()
			return cached.data
		}
		// Expired: keep it in hand as the fallback for a refetch that fails,
		// and leave it in the map so a concurrent caller keeps being served.
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

	data := &TwitchEmoteData{
		BTTV:    bttvResult,
		FFZ:     ffzResult,
		SevenTV: sevenTVResult,
	}
	answered := bttvOK || ffzOK || sevenTVOK

	er.mu.Lock()
	if answered {
		// Refreshing an EXPIRED entry must not evict anything and must not
		// append a second order entry: the key is already in both, and the LRU
		// promotion above already moved it to the end.
		if _, existing := er.cache[cacheKey]; !existing {
			er.evictIfFullLocked()
			er.cacheOrder = append(er.cacheOrder, cacheKey)
		}
		er.cache[cacheKey] = emoteCacheEntry{data: data, fetchedAt: er.now()}
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
			Emoticons []struct {
				ID   int               `json:"id"`
				Name string            `json:"name"`
				URLs map[string]string `json:"urls"`
			} `json:"emoticons"`
		} `json:"sets"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("ffz parse failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var emotes []EmoteInfo
	for _, set := range resp.Sets {
		for _, e := range set.Emoticons {
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
	}

	return emotes, true
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
			Emotes []struct {
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
			} `json:"emotes"`
		} `json:"emote_set"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		er.logger.Warn("7tv parse failed", "err", err, "channelID", channelID)
		return nil, false
	}

	var emotes []EmoteInfo
	for _, e := range resp.EmoteSet.Emotes {
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

	return emotes, true
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

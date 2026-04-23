package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

const emoteTimeout = 8 * time.Second

// EmoteResolver fetches and caches third-party emotes for Twitch channels.
type EmoteResolver struct {
	mu         sync.Mutex
	cache      map[string]*TwitchEmoteData // channelLogin (lowered) -> emotes
	cacheOrder []string                     // insertion order for LRU eviction
	inflight   map[string]chan struct{}      // dedup concurrent fetches for same key

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
		cache:    make(map[string]*TwitchEmoteData),
		inflight: make(map[string]chan struct{}),
		logger:   logger,
	}
}

// Resolve fetches all third-party emotes for a channel.
// Results are cached per channel login (lowercased). The channelID is used for API calls,
// while channelLogin is used as the cache key (matching TypeScript behavior).
func (er *EmoteResolver) Resolve(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
	// Determine cache key: prefer channelLogin, fall back to channelID
	cacheKey := channelID
	if len(channelLogin) > 0 && channelLogin[0] != "" {
		cacheKey = strings.ToLower(channelLogin[0])
	}

	er.mu.Lock()
	if cached, ok := er.cache[cacheKey]; ok {
		// LRU: move to end of order list
		for i, k := range er.cacheOrder {
			if k == cacheKey {
				er.cacheOrder = append(er.cacheOrder[:i], er.cacheOrder[i+1:]...)
				er.cacheOrder = append(er.cacheOrder, cacheKey)
				break
			}
		}
		er.mu.Unlock()
		return cached
	}
	// Dedup: if another goroutine is already fetching this key, wait for it.
	// Respect ctx so a cancelled download doesn't sit here waiting on a
	// fetcher that may itself be blocked on network IO.
	if wait, ok := er.inflight[cacheKey]; ok {
		er.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil
		}
		// Now it should be in cache (unless ctx cancelled and fetcher hadn't
		// populated yet — in which case cache miss is fine, Twitch emote
		// resolution is best-effort).
		er.mu.Lock()
		cached := er.cache[cacheKey]
		er.mu.Unlock()
		return cached
	}
	// Mark this key as inflight
	done := make(chan struct{})
	er.inflight[cacheKey] = done
	er.mu.Unlock()

	er.logger.Debug("resolving emotes", "channelID", channelID)

	// Fetch all providers in parallel
	var wg sync.WaitGroup
	var bttvResult []EmoteInfo
	var ffzResult []EmoteInfo
	var sevenTVResult []EmoteInfo

	wg.Add(3)

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("BTTV emote fetch panic", "panic", r)
			}
		}()
		bttvResult = er.fetchBTTV(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("FFZ emote fetch panic", "panic", r)
			}
		}()
		ffzResult = er.fetchFFZ(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("7TV emote fetch panic", "panic", r)
			}
		}()
		sevenTVResult = er.fetch7TV(ctx, channelID)
	}()

	wg.Wait()

	data := &TwitchEmoteData{
		BTTV:    bttvResult,
		FFZ:     ffzResult,
		SevenTV: sevenTVResult,
	}

	er.mu.Lock()
	// Limit cache size — LRU eviction (oldest first by insertion order)
	if len(er.cache) >= 200 && len(er.cacheOrder) > 0 {
		// Evict the oldest entry
		oldest := er.cacheOrder[0]
		er.cacheOrder = er.cacheOrder[1:]
		delete(er.cache, oldest)
	}
	er.cache[cacheKey] = data
	er.cacheOrder = append(er.cacheOrder, cacheKey)
	delete(er.inflight, cacheKey)
	er.mu.Unlock()
	// Release er.mu before close(done) so any waiting goroutines wake up
	// and re-acquire er.mu cleanly without contending against the still-
	// held lock. Minor throughput win under high concurrent resolve rates.
	close(done)

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
	er.cache = make(map[string]*TwitchEmoteData)
	er.cacheOrder = nil
	er.mu.Unlock()
}

func (er *EmoteResolver) fetchBTTV(ctx context.Context, channelID string) []EmoteInfo {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.BTTVChannel, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		er.logger.Debug("bttv fetch failed", "err", err)
		return nil
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
		er.logger.Debug("bttv parse failed", "err", err)
		return nil
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

	return emotes
}

func (er *EmoteResolver) fetchFFZ(ctx context.Context, channelID string) []EmoteInfo {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.FFZChannel, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		er.logger.Debug("ffz fetch failed", "err", err)
		return nil
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
		er.logger.Debug("ffz parse failed", "err", err)
		return nil
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

	return emotes
}

func (er *EmoteResolver) fetch7TV(ctx context.Context, channelID string) []EmoteInfo {
	url := fmt.Sprintf("%s/%s", constants.TwitchEmoteAPIs.SevenTVUser, channelID)

	ctx, cancel := context.WithTimeout(ctx, emoteTimeout)
	defer cancel()

	data, err := fetchJSON(ctx, url)
	if err != nil {
		er.logger.Debug("7tv fetch failed", "err", err)
		return nil
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
		er.logger.Debug("7tv parse failed", "err", err)
		return nil
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

	return emotes
}

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

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
}

package twitch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// isGlobalEmoteURL reports whether url is one of the three global-set
// endpoints (as opposed to a per-channel lookup).
func isGlobalEmoteURL(url string) bool {
	return url == constants.TwitchEmoteAPIs.BTTVGlobal ||
		url == constants.TwitchEmoteAPIs.FFZGlobal ||
		url == constants.TwitchEmoteAPIs.SevenTVGlobal
}

// emoteRoutes is a stub for the six emote endpoints: a body per URL prefix,
// and a per-URL request count. A URL with no body is a transport failure.
type emoteRoutes struct {
	mu     sync.Mutex
	bodies map[string]string
	counts map[string]int
}

func installEmoteRoutes(t *testing.T, bodies map[string]string) *emoteRoutes {
	t.Helper()
	r := &emoteRoutes{bodies: bodies, counts: map[string]int{}}
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		url := req.URL.String()
		r.mu.Lock()
		defer r.mu.Unlock()
		for prefix, body := range r.bodies {
			if strings.HasPrefix(url, prefix) {
				r.counts[prefix]++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			}
		}
		r.counts[url]++
		return nil, fmt.Errorf("emote provider unreachable")
	})}
	return r
}

func (r *emoteRoutes) set(prefix, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if body == "" {
		delete(r.bodies, prefix)
		return
	}
	r.bodies[prefix] = body
}

func (r *emoteRoutes) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[prefix]
}

func (r *emoteRoutes) globalCount() int {
	api := constants.TwitchEmoteAPIs
	return r.count(api.BTTVGlobal) + r.count(api.FFZGlobal) + r.count(api.SevenTVGlobal)
}

func emoteCodes(list []EmoteInfo) string {
	codes := make([]string, len(list))
	for i, e := range list {
		codes[i] = e.Code
	}
	return strings.Join(codes, ",")
}

const (
	bttvGlobalBody = `[{"id":"g1","code":"FeelsBadMan"},{"id":"g2","code":"Shared"}]`
	// Set 3 is a default set; set 4 is listed but not on for everyone.
	ffzGlobalBody = `{"default_sets":[3],"sets":{` +
		`"3":{"id":3,"emoticons":[{"id":7,"name":"ZreknarF","urls":{"1":"//cdn.frankerfacez.com/7/1","2":"//cdn.frankerfacez.com/7/2"}},{"id":8,"name":"Shared","urls":{"1":"//cdn.frankerfacez.com/8/1"}}]},` +
		`"4":{"id":4,"emoticons":[{"id":9,"name":"NotDefault","urls":{"1":"//cdn.frankerfacez.com/9/1"}}]}}}`
	sevenTVGlobalBody = `{"id":"global","emotes":[{"id":"s1","name":"EZ","data":{"host":{"url":"//cdn.7tv.app/emote/s1","files":[{"name":"1x.webp"},{"name":"2x.webp"}]}}},` +
		`{"id":"s2","name":"catJAM","data":{"host":{"url":"//cdn.7tv.app/emote/s2","files":[{"name":"2x.webp"}]}}}]}`
)

func allEmoteBodies() map[string]string {
	api := constants.TwitchEmoteAPIs
	return map[string]string{
		api.BTTVChannel:   `{"channelEmotes":[{"id":"c1","code":"catJAM"}],"sharedEmotes":[]}`,
		api.FFZChannel:    `{"sets":{"1":{"emoticons":[{"id":5,"name":"chanFFZ","urls":{"2":"//cdn.frankerfacez.com/5/2"}}]}}}`,
		api.SevenTVUser:   `{"emote_set":{"emotes":[]}}`,
		api.BTTVGlobal:    bttvGlobalBody,
		api.FFZGlobal:     ffzGlobalBody,
		api.SevenTVGlobal: sevenTVGlobalBody,
	}
}

// A resolve returns the channel's sets followed by the global ones, parsed
// from each provider's own shape; a global emote whose name a channel emote
// of ANY provider already has is left out (7TV's global catJAM loses to the
// channel's BTTV catJAM), and FFZ contributes its default sets only.
func TestResolveIncludesGlobalEmotesAndTheChannelWins(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	installEmoteRoutes(t, allEmoteBodies())

	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if got == nil {
		t.Fatal("Resolve returned nil")
	}
	if c := emoteCodes(got.BTTV); c != "catJAM,FeelsBadMan,Shared" {
		t.Errorf("BTTV = %s, want the channel emote then the two globals", c)
	}
	if c := emoteCodes(got.FFZ); c != "chanFFZ,ZreknarF,Shared" {
		t.Errorf("FFZ = %s, want the channel emote then default set 3 only", c)
	}
	if c := emoteCodes(got.SevenTV); c != "EZ" {
		t.Errorf("SevenTV = %s, want EZ only: the global catJAM loses to the channel's BTTV catJAM", c)
	}
	want := map[string]string{
		"FeelsBadMan": "https://cdn.betterttv.net/emote/g1/2x.webp",
		"ZreknarF":    "https://cdn.frankerfacez.com/7/2",
		"EZ":          "https://cdn.7tv.app/emote/s1/2x.webp",
	}
	for _, list := range [][]EmoteInfo{got.BTTV, got.FFZ, got.SevenTV} {
		for _, e := range list {
			if u, ok := want[e.Code]; ok && e.URL != u {
				t.Errorf("%s URL = %s, want %s", e.Code, e.URL, u)
			}
		}
	}

	// The cached channel entry is not grown by the merge.
	er.mu.Lock()
	cached := er.cache["chan-1"].data
	er.mu.Unlock()
	if c := emoteCodes(cached.BTTV); c != "catJAM" {
		t.Errorf("the cached channel set was modified: BTTV = %s", c)
	}
}

// The global sets are fetched once and served from cache to every channel
// for 24 hours, then fetched again.
func TestGlobalEmotesAreCachedForTheTTL(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }
	routes := installEmoteRoutes(t, allEmoteBodies())

	er.Resolve(context.Background(), "chan-1", "chan-1")
	er.Resolve(context.Background(), "chan-2", "chan-2")
	if n := routes.globalCount(); n != 3 {
		t.Fatalf("two channels made %d global requests, want 3 (one per provider)", n)
	}
	now = now.Add(emoteCacheTTL - time.Minute)
	er.Resolve(context.Background(), "chan-3", "chan-3")
	if n := routes.globalCount(); n != 3 {
		t.Fatalf("inside the TTL the total went to %d global requests, want 3", n)
	}
	now = now.Add(2 * time.Minute)
	got := er.Resolve(context.Background(), "chan-4", "chan-4")
	if n := routes.globalCount(); n != 6 {
		t.Errorf("past the TTL the total is %d global requests, want 6", n)
	}
	if got == nil || !strings.Contains(emoteCodes(got.SevenTV), "EZ") {
		t.Errorf("refreshed resolve = %+v, want the global sets", got)
	}
}

// A global fetch that fails never fails the resolve: the channel's own sets
// come back alone. The failure is not cached as an answer, and it is not
// retried on every resolve either, only after the retry floor.
func TestGlobalEmoteFailureLeavesTheChannelSetAndIsRetriedLater(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }
	api := constants.TwitchEmoteAPIs
	bodies := allEmoteBodies()
	delete(bodies, api.BTTVGlobal)
	delete(bodies, api.FFZGlobal)
	delete(bodies, api.SevenTVGlobal)
	routes := installEmoteRoutes(t, bodies)

	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if got == nil || emoteCodes(got.BTTV) != "catJAM" || emoteCodes(got.FFZ) != "chanFFZ" || len(got.SevenTV) != 0 {
		t.Fatalf("Resolve with the global endpoints down = %+v, want the channel's own sets", got)
	}
	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := routes.globalCount(); n != 3 {
		t.Fatalf("a second resolve straight after the failure made the total %d global requests, want 3", n)
	}

	routes.set(api.BTTVGlobal, bttvGlobalBody)
	routes.set(api.FFZGlobal, ffzGlobalBody)
	routes.set(api.SevenTVGlobal, sevenTVGlobalBody)
	now = now.Add(globalEmoteRetryFloor)
	got = er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := routes.globalCount(); n != 6 {
		t.Errorf("after the retry floor the total is %d global requests, want 6", n)
	}
	if got == nil || emoteCodes(got.BTTV) != "catJAM,FeelsBadMan,Shared" {
		t.Errorf("after the providers came back Resolve = %+v, want the globals included", got)
	}
}

// When a refresh of an expired global set fails, the set it had is still
// served; a provider that does answer is refreshed on its own.
func TestGlobalEmoteRefreshFailureServesTheStaleSet(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }
	api := constants.TwitchEmoteAPIs
	routes := installEmoteRoutes(t, allEmoteBodies())
	er.Resolve(context.Background(), "chan-1", "chan-1")

	now = now.Add(emoteCacheTTL + time.Minute)
	routes.set(api.BTTVGlobal, "")
	routes.set(api.SevenTVGlobal, `{"id":"global","emotes":[{"id":"s9","name":"NewOne","data":{"host":{"url":"//cdn.7tv.app/emote/s9","files":[{"name":"2x.webp"}]}}}]}`)
	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if got == nil {
		t.Fatal("Resolve returned nil")
	}
	if c := emoteCodes(got.BTTV); c != "catJAM,FeelsBadMan,Shared" {
		t.Errorf("BTTV = %s, want yesterday's global set still served after its refresh failed", c)
	}
	if c := emoteCodes(got.SevenTV); c != "NewOne" {
		t.Errorf("SevenTV = %s, want the refreshed set", c)
	}
}

// With no channel answer and nothing cached the result stays nil, whatever
// the global sets hold, so the per-job cache retries the channel later.
func TestResolveStaysNilWhenTheChannelCannotBeResolved(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	api := constants.TwitchEmoteAPIs
	installEmoteRoutes(t, map[string]string{
		api.BTTVGlobal: bttvGlobalBody, api.FFZGlobal: ffzGlobalBody, api.SevenTVGlobal: sevenTVGlobalBody,
	})
	if got := er.Resolve(context.Background(), "chan-1", "chan-1"); got != nil {
		t.Errorf("Resolve = %+v, want nil", got)
	}
}

func TestWithGlobalEmotes(t *testing.T) {
	channel := &TwitchEmoteData{FFZ: []EmoteInfo{{ID: "1", Code: "A"}}}
	if got := withGlobalEmotes(channel, nil); got != channel {
		t.Error("nil globals must return the channel set itself")
	}
	if got := withGlobalEmotes(channel, &TwitchEmoteData{}); got != channel {
		t.Error("empty globals must return the channel set itself")
	}
	if got := withGlobalEmotes(nil, &TwitchEmoteData{BTTV: []EmoteInfo{{Code: "G"}}}); got != nil {
		t.Error("a nil channel set stays nil")
	}
	got := withGlobalEmotes(channel, &TwitchEmoteData{
		BTTV:    []EmoteInfo{{ID: "g", Code: "A"}, {ID: "g2", Code: "B"}},
		SevenTV: []EmoteInfo{{ID: "g3", Code: "A"}},
	})
	if emoteCodes(got.BTTV) != "B" || emoteCodes(got.FFZ) != "A" || got.SevenTV != nil {
		t.Errorf("merged = %+v", got)
	}
	if len(channel.FFZ) != 1 || channel.BTTV != nil {
		t.Errorf("the channel set was modified: %+v", channel)
	}
}

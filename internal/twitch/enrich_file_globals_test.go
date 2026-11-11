package twitch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// warnRecorder is a logger that keeps its Warn messages.
type warnRecorder struct {
	testLogger
	mu    sync.Mutex
	warns []string
}

func (l *warnRecorder) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *warnRecorder) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

func globalEndpointsDown() map[string]string {
	api := constants.TwitchEmoteAPIs
	bodies := allEmoteBodies()
	delete(bodies, api.BTTVGlobal)
	delete(bodies, api.FFZGlobal)
	delete(bodies, api.SevenTVGlobal)
	return bodies
}

// A caller whose own ctx is already over has not found a provider down: the
// attempt is taken back, so a resolve straight after it, on a live ctx,
// fetches the globals instead of sitting out the retry floor without them.
//
// Mutant: leaving triedAt stamped when the caller's ctx ended (the second
// resolve makes no global request and returns the channel's sets alone).
func TestGlobalEmoteFetchCancelledByItsCallerIsNotAProviderFailure(t *testing.T) {
	logger := &warnRecorder{}
	er := NewEmoteResolver(logger)
	now := time.Now()
	er.now = func() time.Time { return now }
	routes := installEmoteRoutes(t, allEmoteBodies())

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got := er.Resolve(gone, "chan-1", "chan-1"); got != nil {
		t.Fatalf("Resolve on a cancelled ctx = %+v, want nil", got)
	}
	if n := routes.globalCount(); n != 0 {
		t.Fatalf("a cancelled resolve reached the providers %d times, want 0", n)
	}
	for _, line := range logger.warnings() {
		if strings.Contains(line, "global emote set unavailable") {
			t.Errorf("the caller's cancellation was logged as a provider failure: %s", line)
		}
	}

	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := routes.globalCount(); n != 3 {
		t.Errorf("the resolve after a cancelled one made %d global requests, want 3", n)
	}
	if got == nil || emoteCodes(got.BTTV) != "catJAM,FeelsBadMan,Shared" {
		t.Errorf("the resolve after a cancelled one = %+v, want the globals included", got)
	}
}

// A refetch cancelled by its caller keeps an earlier failure's retry floor as
// it was: taking the attempt back restores the previous triedAt, it does not
// clear it.
func TestGlobalEmoteCancelledRefetchKeepsAnEarlierFailuresFloor(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }
	routes := installEmoteRoutes(t, globalEndpointsDown())

	er.Resolve(context.Background(), "chan-1", "chan-1") // fails: the floor starts
	failedAt := now
	now = now.Add(globalEmoteRetryFloor)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	er.Resolve(gone, "chan-1", "chan-1") // due, but its caller is gone
	er.mu.Lock()
	tried := er.globals[globalBTTV].triedAt
	er.mu.Unlock()
	if !tried.Equal(failedAt) {
		t.Errorf("triedAt after a cancelled refetch = %v, want the earlier failure's %v", tried, failedAt)
	}
	if n := routes.globalCount(); n != 3 {
		t.Errorf("total global requests = %d, want the 3 of the first failure", n)
	}
}

// A job keeps no copy of its first emote resolve. When the global providers
// are down at the first part roll, that part's file carries the channel's
// sets alone; a later part, enriched after the providers are back and their
// retry floor has passed, carries the globals.
//
// Mutant: caching the first non-nil resolve on the downloader (the second
// part's file has no globals either).
func TestEnrichFileGetsTheGlobalsOnceTheirProviderIsBack(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }
	api := constants.TwitchEmoteAPIs
	routes := installEmoteRoutes(t, globalEndpointsDown())

	cd := &ChatDownloader{
		channelID:     "chan-1",
		channelLogin:  "chan-1",
		emoteResolver: er,
		logger:        &testLogger{},
	}
	dir := t.TempDir()
	part := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		data := TwitchChatData{
			Platform: "twitch", ChannelLogin: "chan-1", MessageCount: 1,
			Messages: []TwitchChatMessage{{ID: "m1", AuthorName: "a", Message: "hi", MessageType: "chat"}},
		}
		if err := utils.WriteChatFileAtomic(path, &data); err != nil {
			t.Fatal(err)
		}
		return path
	}
	bttvCodes := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var d TwitchChatData
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s is not valid JSON: %v", path, err)
		}
		if d.Emotes == nil {
			t.Fatalf("%s has no emotes block", path)
		}
		return emoteCodes(d.Emotes.BTTV)
	}

	first := part("part1.chat.json")
	cd.EnrichFile(context.Background(), first)
	if got := bttvCodes(first); got != "catJAM" {
		t.Fatalf("part 1 BTTV emotes = %q, want the channel's own catJAM", got)
	}

	routes.set(api.BTTVGlobal, bttvGlobalBody)
	routes.set(api.FFZGlobal, ffzGlobalBody)
	routes.set(api.SevenTVGlobal, sevenTVGlobalBody)
	now = now.Add(globalEmoteRetryFloor)
	second := part("part2.chat.json")
	cd.EnrichFile(context.Background(), second)
	if got := bttvCodes(second); got != "catJAM,FeelsBadMan,Shared" {
		t.Errorf("part 2 BTTV emotes = %q, want the globals too", got)
	}
	if n := routes.count(api.BTTVChannel); n != 1 {
		t.Errorf("the channel's BTTV set was fetched %d times over two parts, want 1", n)
	}
}

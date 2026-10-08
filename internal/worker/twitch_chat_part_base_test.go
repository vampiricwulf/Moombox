package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// pdtEpoch dates segment 0 of the fake broadcast below; segment s is dated
// pdtEpoch + s seconds. Far from the wall clock on purpose, so a part based on
// the local clock at its start cannot pass for one based on its first segment.
var pdtEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// pdtStream is twoVariantStream with #EXT-X-PROGRAM-DATE-TIME on every
// segment and a shorter window, recording the first segment each variant's
// downloader fetched — the first one it WROTE, on the sequential live path.
type pdtStream struct {
	start           time.Time
	switchAt, endAt time.Duration
	mu              sync.Mutex
	first           map[string]int
}

func (s *pdtStream) firstSeq(name string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.first[name]
	return seq, ok
}

func newPDTStream(t *testing.T) (*pdtStream, twitch.TwitchHLSVariant, twitch.TwitchHLSVariant) {
	t.Helper()
	ts := oneSecondTS(t)
	st := &pdtStream{start: time.Now(), switchAt: 3 * time.Second, endAt: 7 * time.Second, first: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			if seq, err := strconv.Atoi(strings.TrimSuffix(rest, ".ts")); err == nil {
				st.mu.Lock()
				if _, seen := st.first[name]; !seen {
					st.first[name] = seq
				}
				st.mu.Unlock()
			}
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(ts)
			return
		}
		name = strings.TrimSuffix(name, ".m3u8")
		el := time.Since(st.start)
		if name == "720" && el > st.switchAt {
			http.NotFound(w, r)
			return
		}
		head := int(el.Seconds())
		lo := max(0, head-2)
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", lo)
		for seq := lo; seq <= head; seq++ {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:1.000,\n/%s/%d.ts\n",
				pdtEpoch.Add(time.Duration(seq)*time.Second).Format("2006-01-02T15:04:05.000Z07:00"), name, seq)
		}
		if el > st.endAt {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	low := twitch.TwitchHLSVariant{URL: srv.URL + "/720.m3u8", Name: "720p30", Width: 1280, Height: 720, FPS: 30}
	high := twitch.TwitchHLSVariant{URL: srv.URL + "/1080.m3u8", Name: "1080p60", Width: 1920, Height: 1080, FPS: 60}
	return st, low, high
}

// fakeIRC accepts the chat's anonymous handshake, says one PRIVMSG at once
// and a second once `second` returns, then holds the line until the client
// leaves. Points constants.TwitchURLs.IRCWS at itself for the test.
func fakeIRC(t *testing.T, first, second string, wait func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for range 4 { // PASS, NICK, CAP REQ, JOIN
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(first)); err != nil {
			return
		}
		wait()
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(second)); err != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
}

func privmsg(id string, at time.Time) string {
	return fmt.Sprintf("@id=%s;tmi-sent-ts=%d;display-name=Viewer :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #testchan :%s",
		id, at.UnixMilli(), id)
}

// TestTwitchPartChatIsBasedOnItsFirstSegment drives the real ExecuteTwitch
// with a live IRC chat through a quality split, and checks each part's chat
// file is based on the program date-time of the first segment ITS video wrote
// (D-T8): the first part on the 720p downloader's, the part the split opened
// on the 1080p downloader's. Both used to be the local clock at the part's
// start — which here is months away from the playlist's dates, and in
// production is late by however far behind the live edge the window starts.
//
// Mutants: the AwaitPartBase call for the first part dropped (the first part's
// header names the local clock: its message is flushed before the video's
// report can land); advanceToNewPart rolling with RollFile instead of
// RollFileAwaitingBase (the same for the second part); createDownloader not
// wiring OnFirstSegment (both parts keep the local clock, after the wait or at
// the final flush).
func TestTwitchPartChatIsBasedOnItsFirstSegment(t *testing.T) {
	st, low, high := newPDTStream(t)
	msg1At, msg2At := pdtEpoch.Add(100*time.Second), pdtEpoch.Add(200*time.Second)
	fakeIRC(t, privmsg("before-split", msg1At), privmsg("after-split", msg2At), func() {
		time.Sleep(time.Until(st.start.Add(st.switchAt + 1500*time.Millisecond)))
	})

	h := newEndVerdictHarness(t, "tw_chat_pdt")
	h.variant.URL, h.variant.Name, h.variant.Width, h.variant.Height, h.variant.FPS = low.URL, low.Name, low.Width, low.Height, low.FPS
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return time.Since(st.start) <= st.endAt, nil }
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		if time.Since(st.start) > st.switchAt {
			return []twitch.TwitchHLSVariant{high}, nil
		}
		return []twitch.TwitchHLSVariant{low}, nil
	}
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "chatpdt"

	chat := twitch.NewChatDownloader(twitch.ChatDownloaderOptions{
		ChannelLogin: "testchan", StreamID: "s1",
		OutputPath: filepath.Join(h.jobCtx.StagingDir, "chat.json"),
	}, &discardLogger{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, chat); err != nil {
		t.Logf("ExecuteTwitch: %v", err)
	}

	first720, ok720 := st.firstSeq("720")
	first1080, ok1080 := st.firstSeq("1080")
	if !ok720 || !ok1080 {
		t.Fatalf("both variants must be fetched: 720p first %d (%v), 1080p first %d (%v)", first720, ok720, first1080, ok1080)
	}
	for _, part := range []struct {
		path     string
		firstSeq int
		msgAt    time.Time
	}{
		{filepath.Join(h.jobCtx.StagingDir, "chat.json"), first720, msg1At},
		{filepath.Join(h.jobCtx.StagingDir, "seg_0", "chat.json"), first1080, msg2At},
	} {
		raw, err := os.ReadFile(part.path)
		if err != nil {
			t.Errorf("part chat %s: %v", part.path, err)
			continue
		}
		var d twitch.TwitchChatData
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("parse %s: %v", part.path, err)
		}
		base := pdtEpoch.Add(time.Duration(part.firstSeq) * time.Second)
		if d.RecordingStartTime != base.Format(time.RFC3339) {
			t.Errorf("%s: recordingStartTime = %q, want its first segment's %q",
				part.path, d.RecordingStartTime, base.Format(time.RFC3339))
		}
		if len(d.Messages) != 1 || d.Messages[0].OffsetMs != part.msgAt.Sub(base).Milliseconds() {
			t.Errorf("%s: messages %+v, want one at offset %d", part.path, d.Messages, part.msgAt.Sub(base).Milliseconds())
		}
	}
}

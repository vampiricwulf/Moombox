package twitch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Twitch answers a deleted or expired VOD's comments with `"video": null`, and
// can answer `"comments": null`, both with no `errors` array. Decoded into
// value structs those read as zero edges with hasNextPage=false, which is what
// the real last page looks like, so a VOD that vanished mid-paging had its
// chat recorded as finished and its resume sidecar removed.
//
// The one null that stays an empty, clean answer is `comments` on the page
// that enters the VOD at offset 0: a VOD with no chat.
//
// Mutant: decoding video/comments as values again. The two "gone" rows return
// nil and lose their sidecar.
func TestVodChatNullAnswerIsNotTheEndOfChat(t *testing.T) {
	const nullVideo = `{"data":{"video":null}}`
	const nullComments = `{"data":{"video":{"comments":null}}}`
	firstPage := vodCommentsReply(0, vodCommentPageSpec{count: 2, offset: 100, hasNext: true})

	tests := []struct {
		name    string
		replies []string // reply n, the last one repeating
		wantErr error
		wantMsg int
	}{
		{"video null after a page", []string{firstPage, nullVideo}, ErrVodCommentsVideoGone, 2},
		{"video null on the first page", []string{nullVideo}, ErrVodCommentsVideoGone, 0},
		{"comments null on a cursor page", []string{firstPage, nullComments}, ErrVodCommentsMissing, 2},
		{"comments null entering at offset 0", []string{nullComments}, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldBackoff := vodChatErrorBackoff
			vodChatErrorBackoff = time.Millisecond
			prev := twitchHTTPClient
			t.Cleanup(func() { twitchHTTPClient, vodChatErrorBackoff = prev, oldBackoff })

			var requests atomic.Int32
			twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
				n := min(int(requests.Add(1)), len(tt.replies)) - 1
				h := make(http.Header)
				h.Set("Content-Type", "application/json")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(bytes.NewReader([]byte(tt.replies[n]))),
					Request:    req,
				}, nil
			})}

			out := filepath.Join(t.TempDir(), "vod.chat.json")
			vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{VodID: "v1", OutputPath: out}, &testLogger{})
			err := vcd.Start(context.Background())

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Start returned %v, want %v", err, tt.wantErr)
			}
			if got := vcd.MessageCount(); got != tt.wantMsg {
				t.Errorf("MessageCount = %d, want %d", got, tt.wantMsg)
			}
			_, statErr := os.Stat(out + ".resume.json")
			if tt.wantErr != nil && statErr != nil {
				t.Errorf("resume sidecar missing after an unfinished run: %v", statErr)
			}
			if tt.wantErr == nil && statErr == nil {
				t.Error("resume sidecar left behind by a VOD with no chat")
			}
		})
	}
}

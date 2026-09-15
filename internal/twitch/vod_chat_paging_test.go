package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// vodCommentPageSpec is one GQL page of the fake server's script: `count` edges
// that ALL share the same content offset second — the burst that made the
// offset-based pager loop on itself — each carrying its own cursor.
type vodCommentPageSpec struct {
	count   int
	offset  float64
	hasNext bool
}

// vodCommentsReply renders the real VideoCommentsByOffsetOrCursor response
// shape around one page. Edge i of page p gets id "c<p>-<i>" and cursor
// "cur<p>-<i>"; the fake server serves page p+1 when asked for "cur<p>-<last>".
func vodCommentsReply(page int, p vodCommentPageSpec) string {
	edges := make([]map[string]any, 0, p.count)
	for i := range p.count {
		edges = append(edges, map[string]any{
			"cursor": fmt.Sprintf("cur%d-%d", page, i),
			"node": map[string]any{
				"id":                   fmt.Sprintf("c%d-%d", page, i),
				"contentOffsetSeconds": p.offset,
				"commenter":            map[string]any{"displayName": "Viewer", "id": "u1", "login": "viewer"},
				"message": map[string]any{
					"fragments":  []map[string]any{{"text": "hello"}},
					"userBadges": []map[string]any{},
					"userColor":  nil,
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{"video": map[string]any{"comments": map[string]any{
			"edges":    edges,
			"pageInfo": map[string]any{"hasNextPage": p.hasNext},
		}}},
	})
	if err != nil {
		panic(err) // a literal this file built cannot fail to marshal
	}
	return string(body)
}

// installVodCommentStub points the package HTTP client at a fake GQL server
// serving `pages` in order, and returns a recorder of how each page was ASKED
// for ("offset:<v>" or "cursor:<v>"). It reuses probeRoundTripper
// (liveness_probe_test.go) rather than declaring a second identical type.
//
// The swap is why no test in this file may call t.Parallel: twitchHTTPClient is
// shared with every other test in the package.
func installVodCommentStub(t *testing.T, pages []vodCommentPageSpec) *[]string {
	t.Helper()
	var mu sync.Mutex
	asked := make([]string, 0, len(pages))
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })

	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var q struct {
			Variables struct {
				VideoID              string   `json:"videoID"`
				ContentOffsetSeconds *float64 `json:"contentOffsetSeconds"`
				Cursor               *string  `json:"cursor"`
			} `json:"variables"`
			Extensions struct {
				PersistedQuery struct {
					Hash string `json:"sha256Hash"`
				} `json:"persistedQuery"`
			} `json:"extensions"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("stub could not parse the request body: %w", err)
		}
		if q.Extensions.PersistedQuery.Hash != constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor {
			return nil, fmt.Errorf("stub: the persisted-query hash changed")
		}

		page := 0
		switch {
		case q.Variables.Cursor != nil && *q.Variables.Cursor != "":
			cursor := *q.Variables.Cursor
			mu.Lock()
			asked = append(asked, "cursor:"+cursor)
			mu.Unlock()
			// "cur<p>-<i>" asks for page p+1.
			var p, i int
			if _, err := fmt.Sscanf(cursor, "cur%d-%d", &p, &i); err != nil {
				return nil, fmt.Errorf("stub: unrecognised cursor")
			}
			page = p + 1
		case q.Variables.ContentOffsetSeconds != nil:
			mu.Lock()
			asked = append(asked, fmt.Sprintf("offset:%v", *q.Variables.ContentOffsetSeconds))
			mu.Unlock()
		default:
			return nil, fmt.Errorf("stub: the request carried neither an offset nor a cursor")
		}
		if page >= len(pages) {
			return nil, fmt.Errorf("stub: asked for page %d of %d — the loop ran past the script", page, len(pages))
		}

		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       io.NopCloser(bytes.NewReader([]byte(vodCommentsReply(page, pages[page])))),
			Request:    req,
		}, nil
	})}
	return &asked
}

// TestVodChatPagesByCursorThroughABurstSecond is T1-3.
//
// A second of a popular VOD can hold more comments than one page (59 edges),
// and every edge in that second reports the SAME integer contentOffsetSeconds.
// The offset-based pager therefore asked for the same page again, saw only
// duplicates, found the offset had not advanced, and BROKE — stranding the
// whole rest of the VOD's chat behind that one second.
//
// Mutants this kills:
//   - paging by offset (today): 59 messages archived, the run stops with the
//     "offset did not advance" Warn.
//   - paging by the FIRST edge's cursor instead of the last: page 2 repeats
//     58 of page 1's edges and the count lands short.
//   - sending both variables: the stub's request classifier is a real
//     discriminator — Twitch's own resolver prefers one, and shipping both
//     leaves which one undefined.
func TestVodChatPagesByCursorThroughABurstSecond(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 59, offset: 1200, hasNext: true},
		{count: 59, offset: 1200, hasNext: true},
		{count: 10, offset: 1200, hasNext: false},
	}
	asked := installVodCommentStub(t, pages)

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := vcd.MessageCount(); got != 128 {
		t.Errorf("archived %d comments, want 128 — the tail past the burst second was stranded", got)
	}
	want := []string{"offset:0", "cursor:cur0-58", "cursor:cur1-58"}
	if !slices.Equal(*asked, want) {
		t.Errorf("pages were asked for as %v, want %v", *asked, want)
	}
}

// TestVodChatResumeStartsFromTheSavedOffset pins the half that stays offset-
// based: a cursor is opaque and is not persisted, so a resumed run re-enters by
// LastOffsetSeconds and switches to cursors from its second page on.
//
// Mutant: persisting the cursor in the sidecar and resuming from it — Twitch's
// cursors are not documented as durable across sessions, and a stale one
// returns an empty page that today's `len(edges) == 0` arm reads as "end of
// VOD".
func TestVodChatResumeStartsFromTheSavedOffset(t *testing.T) {
	pages := []vodCommentPageSpec{{count: 2, offset: 900, hasNext: false}}
	asked := installVodCommentStub(t, pages)

	dir := t.TempDir()
	out := filepath.Join(dir, "vod.chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})
	vcd.saveResumeState(900)

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "offset:900" {
		t.Errorf("resumed run asked %v, want a single offset:900 request", *asked)
	}
}

// TestVodChatStopsOnAStuckCursor replaces the offset-advance guard the cursor
// pager retires. A server that answers every cursor with the same page and
// hasNextPage=true would otherwise spin forever.
//
// Mutant: deleting the guard with nothing in its place. The fake server's
// "ran past the script" error would eventually end the run through the
// consecutive-error budget, which is why the assertion is on the REQUEST COUNT,
// not merely on termination.
func TestVodChatStopsOnAStuckCursor(t *testing.T) {
	var mu sync.Mutex
	var requests int
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		mu.Unlock()
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		// Always page 0, always hasNext — a cursor that never advances.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body: io.NopCloser(bytes.NewReader([]byte(
				vodCommentsReply(0, vodCommentPageSpec{count: 3, offset: 60, hasNext: true})))),
			Request: req,
		}, nil
	})}

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})
	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 2 {
		t.Errorf("made %d requests against a stuck cursor, want 2 (the page, then the repeat "+
			"that proves the cursor did not move)", got)
	}
}

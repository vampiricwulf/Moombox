package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	// noCursor renders every edge's "cursor" field as "" — a schema surprise
	// (fix round R4) distinct from the burst second above: Twitch says more
	// pages exist (hasNextPage=true) but the edges carry nothing to page by.
	noCursor bool
}

// vodCommentsReply renders the real VideoCommentsByOffsetOrCursor response
// shape around one page. Edge i of page p gets id "c<p>-<i>" and cursor
// "cur<p>-<i>" (or "" when p.noCursor); the fake server serves page p+1 when
// asked for "cur<p>-<last>".
func vodCommentsReply(page int, p vodCommentPageSpec) string {
	edges := make([]map[string]any, 0, p.count)
	for i := range p.count {
		cursor := fmt.Sprintf("cur%d-%d", page, i)
		if p.noCursor {
			cursor = ""
		}
		edges = append(edges, map[string]any{
			"cursor": cursor,
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
// Fix round R4: a stuck cursor is a PAGING STALL, not completion — Twitch
// still claims more pages exist. Start must now return an error (so the
// caller never treats this VOD's chat as finished) and the resume sidecar
// must survive (so a later /resume retries from here) — previously this
// path broke the loop, returned nil, and removeResumeState() deleted the
// sidecar, exactly the silent-truncation class of bug T1-3/R4 exist to stop.
//
// Mutant: deleting the guard with nothing in its place. The fake server's
// "ran past the script" error would eventually end the run through the
// consecutive-error budget, which is why one assertion is on the REQUEST
// COUNT, not merely on termination.
//
// Fix round R4 follow-up (round 2): the orchestrator discards Start's
// returned error entirely (internal/worker/orchestrator_twitch.go), so a
// production stall leaves NO trace unless pagingStalled itself logs one.
// The downloader logger is a renderingLogger (api_gql_log_hygiene_test.go)
// instead of the no-op testLogger so that trace can be asserted on.
//
// Mutant: deleting the Warn call inside pagingStalled.
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

	out := filepath.Join(t.TempDir(), "vod.chat.json")
	rl := &renderingLogger{}
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, rl)
	if err := vcd.Start(context.Background()); err == nil {
		t.Fatal("Start returned nil, want an error — a stuck cursor is a stall, not completion " +
			"(mutant: still breaking the loop and returning nil)")
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 2 {
		t.Errorf("made %d requests against a stuck cursor, want 2 (the page, then the repeat "+
			"that proves the cursor did not move)", got)
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Errorf("resume sidecar missing after a stalled stuck-cursor run: %v — "+
			"want it preserved so a later /resume retries from here "+
			"(mutant: removeResumeState() still runs on this path)", err)
	}
	if !strings.Contains(rl.allLines(), "WARN [TwitchVodChat] paging stalled; resume state kept") ||
		!strings.Contains(rl.allLines(), "reason=cursor did not advance") {
		t.Errorf("no Warn logged for the stall (got log lines: %q) — a stuck cursor "+
			"in production would leave no trace at all, since the orchestrator discards "+
			"Start's returned error (mutant: the Warn call inside pagingStalled was deleted)",
			rl.allLines())
	}
}

// TestVodChatFallsBackToOffsetPastAnEmptyCursor is fix round R4(a): Twitch
// can claim more pages exist (hasNextPage=true) while sending an edge with no
// cursor at all — a schema surprise distinct from the burst-second case T1-3
// fixes. That must not be read as completion: the loop falls back to paging
// by the last edge's OFFSET (the pre-T1-3 behaviour) and keeps going.
//
// The fallback is a DETOUR, not a mode switch: the page it fetches by offset
// is cursor-bearing again, and the pager must go straight back to cursor
// paging from it. The script therefore runs four pages — cursor, no-cursor,
// offset-fetched-with-cursors, cursor — and the request sequence is asserted
// in full.
//
// Mutants this kills:
//   - treating the empty cursor as completion (breaking here): the run would
//     stop at 4 archived comments instead of reaching the fourth page.
//   - LATCHING offset paging after the first fallback — clearing the cursor
//     once and never consulting last.Cursor again. Verified: the fourth
//     request becomes offset:300 rather than cursor:cur2-1, which the request
//     sequence names outright, and the stub's terminal answer to an unexpected
//     offset drops the count to 6.
func TestVodChatFallsBackToOffsetPastAnEmptyCursor(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var q struct {
			Variables struct {
				ContentOffsetSeconds *float64 `json:"contentOffsetSeconds"`
				Cursor               *string  `json:"cursor"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("stub could not parse the request body: %w", err)
		}

		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		reply := func(page int, p vodCommentPageSpec) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(bytes.NewReader([]byte(vodCommentsReply(page, p)))),
				Request:    req,
			}, nil
		}

		switch {
		case q.Variables.Cursor != nil && *q.Variables.Cursor != "":
			cursor := *q.Variables.Cursor
			mu.Lock()
			asked = append(asked, "cursor:"+cursor)
			mu.Unlock()
			if strings.HasPrefix(cursor, "cur0-") {
				// Page 0's cursor request: answer with the schema-surprise
				// page — more edges, no cursor on any of them, hasNext still
				// true.
				return reply(1, vodCommentPageSpec{count: 2, offset: 200, hasNext: true, noCursor: true})
			}
			// A cursor from the page the OFFSET fallback fetched. Being asked
			// this at all is the point of the fourth page: a pager that
			// latched onto offsets never gets here.
			return reply(3, vodCommentPageSpec{count: 2, offset: 400, hasNext: false})
		case q.Variables.ContentOffsetSeconds != nil:
			offset := *q.Variables.ContentOffsetSeconds
			mu.Lock()
			asked = append(asked, fmt.Sprintf("offset:%v", offset))
			mu.Unlock()
			switch offset {
			case 0:
				// Fresh start: normal cursor-bearing page.
				return reply(0, vodCommentPageSpec{count: 2, offset: 100, hasNext: true})
			case 200:
				// The offset fallback's request, past the no-cursor page. It
				// is cursor-bearing again and still claims more pages, so the
				// run must RESUME cursor paging from its last edge.
				return reply(2, vodCommentPageSpec{count: 2, offset: 300, hasNext: true})
			}
			// Any other offset means the run never left offset paging after
			// the fallback. Answer with a terminal empty page so the mutant
			// ends instead of asking offset:300 forever, and let the
			// request-sequence assertion below be what names it.
			return reply(3, vodCommentPageSpec{count: 0, offset: 400, hasNext: false})
		default:
			return nil, fmt.Errorf("stub: the request carried neither an offset nor a cursor")
		}
	})}

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})
	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := vcd.MessageCount(); got != 8 {
		t.Errorf("archived %d comments, want 8 — the empty-cursor page was read as completion "+
			"instead of falling back to offset paging", got)
	}
	// The fourth entry is the assertion that matters here: cursor paging
	// RESUMED after the offset detour (mutant: a run that stays on offsets
	// once it has fallen back would ask offset:300 instead).
	want := []string{"offset:0", "cursor:cur0-1", "offset:200", "cursor:cur2-1"}
	mu.Lock()
	gotAsked := append([]string(nil), asked...)
	mu.Unlock()
	if !slices.Equal(gotAsked, want) {
		t.Errorf("pages were asked for as %v, want %v", gotAsked, want)
	}
}

// TestVodChatStopsOnZeroEdgesWithMorePagesClaimed is fix round R4(b)'s third
// case: Twitch answers hasNextPage=true with NO edges at all. Reading that as
// "end of VOD" (the pre-fix-round behaviour, shared with the empty-page
// natural-end case) would silently truncate the archive despite Twitch
// saying there is more; it must be treated as a stall like a stuck cursor.
//
// Mutant: the unconditional len(edges)==0 → break-as-complete this replaces.
func TestVodChatStopsOnZeroEdgesWithMorePagesClaimed(t *testing.T) {
	var mu sync.Mutex
	var requests int
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		if n == 1 {
			// First page: normal, with more claimed.
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body: io.NopCloser(bytes.NewReader([]byte(
					vodCommentsReply(0, vodCommentPageSpec{count: 2, offset: 100, hasNext: true})))),
				Request: req,
			}, nil
		}
		// Second page onward: zero edges, but Twitch still claims more exist.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body: io.NopCloser(bytes.NewReader([]byte(
				vodCommentsReply(1, vodCommentPageSpec{count: 0, offset: 100, hasNext: true})))),
			Request: req,
		}, nil
	})}

	out := filepath.Join(t.TempDir(), "vod.chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})
	if err := vcd.Start(context.Background()); err == nil {
		t.Fatal("Start returned nil, want an error — zero edges with hasNextPage=true is a stall, " +
			"not completion (mutant: unconditional break-as-complete on len(edges)==0)")
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Errorf("resume sidecar missing after a stalled zero-edges run: %v — want it preserved", err)
	}
}

// TestVodChatStartReportsARecoveredPanic pins a panic as an outcome: Start
// recovers it and used to return nil, so the worker recorded chat_status
// "finished" over a VOD capture that died.
func TestVodChatStartReportsARecoveredPanic(t *testing.T) {
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &panicOnceLogger{}) // panics on Start's first Info line

	if err := vcd.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Start returned %v after a recovered panic, want an error naming the panic", err)
	}
}

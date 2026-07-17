package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// This file is the InnerTube /browse continuation client for the feed-history
// backfill (spec §11): page a channel tab newest-first, one page per call, the
// scanner owning the loop, cadence and stop conditions. Transport is modeled
// on internal/chat's continuation paging (api.go fetchChat); yt-dlp _tab.py is
// the reference for the response SHAPE only — token at
// continuationItemRenderer.continuationEndpoint.continuationCommand.token,
// page-2+ items under
// onResponseReceivedActions[].appendContinuationItemsAction.continuationItems[].

// ErrContinuationLoop reports that a /browse page handed back a continuation
// token this scan has already seen. YouTube feeds occasionally loop instead of
// ending (yt-dlp guards the same way with its seen_continuations set); without
// the sentinel the scanner would page one channel forever.
var ErrContinuationLoop = errors.New("browse continuation loop detected")

// maxBrowseResponseBytes caps a /browse response read. Channel tab pages are
// far smaller than watch pages; 10 MB matches the player API's cap.
const maxBrowseResponseBytes = 10 << 20

// browseTabParams maps a tab name to the /browse `params` blob that selects
// it: a protobuf with field 2 (string) = the tab's URL path segment,
// base64-encoded. "EgZ2aWRlb3M=" (videos) and "EgdzdHJlYW1z" (streams) are the
// long-standing public InnerTube constants; "EgptZW1iZXJzaGlw" (membership) is
// the same derivation applied to the /membership path that
// channel_membership.go fetches as HTML (`\x12\x0a` + "membership").
var browseTabParams = map[string]string{
	"videos":     "EgZ2aWRlb3M=",
	"streams":    "EgdzdHJlYW1z",
	"membership": "EgptZW1iZXJzaGlw",
}

// TabItem is one video listed on a channel tab page. Age comes from itemAge
// (spec §11 classification): 0 means live/upcoming/undatable — "now" — via the
// live-badge short-circuit; only a "Streamed N <unit> ago" text yields a
// non-zero coarse age (the lower bound of the true age, spec §12).
type TabItem struct {
	VideoID string
	Title   string
	Age     time.Duration
}

// TabPage is one /browse page of a channel tab. Continuation "" means the tab
// is exhausted — the caller stops paging.
type TabPage struct {
	Items        []TabItem
	Continuation string
}

// browseState is the Service-owned cross-page state for tab scans, keyed by
// (channelID, tab). Two things must survive between FetchChannelTabPage calls
// of the same scan:
//
//   - seen continuation tokens, for loop detection (a loop can cycle A→B→A,
//     so comparing only request vs. response token is not enough);
//   - visitorData re-extracted from each response — reusing a stale value
//     kills continuations mid-scan (the chat downloader's stale-token
//     recovery exists for the same underlying failure; see also
//     ytdl-org/youtube-dl#28702, referenced by yt-dlp's _tab.py).
//
// The zero value is usable (tests build bare Services). Sessions are dropped
// when a tab exhausts or loops, so a 24/7 process holds at most one small
// entry per actively-scanning (channel, tab).
type browseState struct {
	mu       sync.Mutex
	sessions map[string]*browseSession
	endpoint string // test override; "" = constants.YouTubeURLs.API + "/browse"
}

type browseSession struct {
	seen        map[string]struct{}
	visitorData string
}

// beginPage returns the visitorData to use for this page's request and
// prepares the (channelID, tab) session. A page-1 call (continuation == "")
// RESETS the session: the cursor-lifecycle rule (spec §11) says a restarted
// scan never resumes stale loop/visitor state — a crash-resume repopulates the
// seen set as it pages. The request's own token is recorded as seen so a page
// that echoes it back is caught immediately, even on a fresh session resumed
// from a DB cursor.
func (b *browseState) beginPage(key, continuation, fallbackVisitorData string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions == nil {
		b.sessions = make(map[string]*browseSession)
	}
	sess := b.sessions[key]
	if continuation == "" || sess == nil {
		sess = &browseSession{seen: make(map[string]struct{})}
		b.sessions[key] = sess
	}
	if continuation != "" {
		sess.seen[continuation] = struct{}{}
	}
	if sess.visitorData != "" {
		return sess.visitorData
	}
	return fallbackVisitorData
}

// finishPage records a fetched page's outcome: stores the response's
// visitorData for the next page, detects token loops, and drops the session
// when the tab ends (exhaustion or loop) so state stays bounded.
func (b *browseState) finishPage(key, token, visitorData string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess := b.sessions[key]
	if sess == nil { // page-1 reset always creates one; defensive only
		return nil
	}
	if visitorData != "" {
		sess.visitorData = visitorData
	}
	if token == "" {
		delete(b.sessions, key)
		return nil
	}
	if _, dup := sess.seen[token]; dup {
		delete(b.sessions, key)
		return ErrContinuationLoop
	}
	sess.seen[token] = struct{}{}
	return nil
}

// FetchChannelTabPage fetches ONE page of a channel tab via the InnerTube
// /browse API. tab ∈ {"videos", "streams", "membership"}. Pass continuation ""
// for the first page (browseId + per-tab params); pass the previous page's
// Continuation for the next. Continuation "" in the result means the tab is
// exhausted.
//
// Auth: cookies ride every request via GenerateAPIHeaders — that is what
// unlocks members content on the membership tab, exactly like
// FetchMembershipVideos' authed page fetch. The HasAuthCookies eligibility
// gate deliberately stays the CALLER's job (the scanner gates which tabs are
// eligible, spec §11); called without cookies the membership tab simply has no
// selected TAB_ID_SPONSORSHIPS tab and returns an empty, exhausted page.
//
// A page handing back an already-seen continuation token returns
// ErrContinuationLoop (wrapped); the scan session for that tab is dropped so a
// retry starts clean.
func (s *Service) FetchChannelTabPage(ctx context.Context, channelID, tab, continuation string) (*TabPage, error) {
	params, ok := browseTabParams[tab]
	if !ok {
		return nil, fmt.Errorf("unknown channel tab %q", tab)
	}

	// Cookies may have rotated since the last page (a wide-window scan runs
	// for minutes); same warn-only sync as FetchMembershipVideos.
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before browse fetch", "error", err)
	}

	sessKey := channelID + "\x00" + tab
	visitorData := s.browse.beginPage(sessKey, continuation, s.GetVisitorData())

	var (
		items    []MembershipVideo
		token    string
		newVD    string
		fetchErr error
	)
	if continuation == "" {
		items, token, newVD, fetchErr = s.fetchBrowseFirstPage(ctx, channelID, tab, params, visitorData)
	} else {
		items, token, newVD, fetchErr = s.fetchBrowseContinuation(ctx, continuation, visitorData)
	}
	if fetchErr != nil {
		return nil, fmt.Errorf("browse %s tab %s: %w", channelID, tab, fetchErr)
	}

	if err := s.browse.finishPage(sessKey, token, newVD); err != nil {
		return nil, fmt.Errorf("browse %s tab %s: %w", channelID, tab, err)
	}

	page := &TabPage{Continuation: token, Items: make([]TabItem, 0, len(items))}
	for _, v := range items {
		page.Items = append(page.Items, TabItem{VideoID: v.VideoID, Title: v.Title, Age: v.Age})
	}
	return page, nil
}

// fetchBrowseFirstPage POSTs browseId+params and parses the ytInitialData-
// shaped response (the /browse page-1 body carries the same
// contents.twoColumnBrowseResultsRenderer.tabs envelope the HTML page embeds).
// When the videos tab yields nothing usable it falls back to browsing the
// channel's UU uploads playlist — topic/auto-generated channels have no
// /videos tab but do have the equivalent playlist (yt-dlp does the same
// redirect; browseId "VL<playlist>" per _tab.py). walkVideoRenderers already
// handles the playlist's playlistVideoRenderer items.
func (s *Service) fetchBrowseFirstPage(ctx context.Context, channelID, tab, params, visitorData string) (items []MembershipVideo, token, newVD string, err error) {
	body, err := s.browsePost(ctx, s.browseRequestBody(channelID, params, "", visitorData), visitorData)
	if err != nil {
		return nil, "", "", err
	}
	items, token, newVD, hasTab := parseBrowseTabPage(body, tab)

	// UC→UU uploads-playlist fallback, videos tab only: "streams" or
	// "membership" legitimately not existing is natural exhaustion (a channel
	// that never streamed / a non-member), never a reason to scan uploads. An
	// empty-but-present videos tab also triggers the fallback — for a truly
	// empty channel the playlist is empty too (one wasted request), and for a
	// tab-less channel it is the only source. Selection depth cannot tell
	// "channel lacks /videos" apart from "videos tab is empty", so the
	// zero-yield trigger covers both.
	if tab == "videos" && (!hasTab || (len(items) == 0 && token == "")) && strings.HasPrefix(channelID, "UC") {
		plBody, plErr := s.browsePost(ctx, s.browseRequestBody("VLUU"+channelID[2:], "", "", visitorData), visitorData)
		if plErr != nil {
			return nil, "", "", fmt.Errorf("uploads-playlist fallback: %w", plErr)
		}
		if plItems, plToken, plVD, plOK := parseBrowseTabPage(plBody, tab); plOK {
			items, token = plItems, plToken
			if plVD != "" {
				newVD = plVD
			}
		}
	}
	return items, token, newVD, nil
}

// fetchBrowseContinuation POSTs a continuation token and parses the page-2+
// envelope: onResponseReceivedActions (or the onResponseReceivedEndpoints
// variant yt-dlp also accepts) → appendContinuationItemsAction →
// continuationItems.
func (s *Service) fetchBrowseContinuation(ctx context.Context, continuation, visitorData string) (items []MembershipVideo, token, newVD string, err error) {
	body, err := s.browsePost(ctx, s.browseRequestBody("", "", continuation, visitorData), visitorData)
	if err != nil {
		return nil, "", "", err
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", "", fmt.Errorf("parse browse continuation response: %w", err)
	}
	seen := make(map[string]struct{})
	for _, key := range []string{"onResponseReceivedActions", "onResponseReceivedEndpoints"} {
		actions, ok := root[key].([]any)
		if !ok {
			continue
		}
		for _, a := range actions {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			app, ok := am["appendContinuationItemsAction"].(map[string]any)
			if !ok {
				continue
			}
			cont, ok := app["continuationItems"].([]any)
			if !ok {
				continue
			}
			walkVideoRenderers(cont, seen, &items)
			if token == "" {
				token = findContinuationToken(cont)
			}
		}
	}
	return items, token, getDeepStr(root, "responseContext", "visitorData"), nil
}

// browseRequestBody builds the /browse POST body: WEB client context (plus
// visitorData when known — same shape as fetchWithClient's clientCtx), then
// either browseId+params (page 1) or the bare continuation (page 2+), matching
// internal/chat's fetchChat request.
func (s *Service) browseRequestBody(browseID, params, continuation, visitorData string) map[string]any {
	clientCtx := make(map[string]any, len(constants.WebClient.Context)+1)
	maps.Copy(clientCtx, constants.WebClient.Context)
	if visitorData != "" {
		clientCtx["visitorData"] = visitorData
	}
	body := map[string]any{
		"context": map[string]any{"client": clientCtx},
	}
	if continuation != "" {
		body["continuation"] = continuation
		return body
	}
	body["browseId"] = browseID
	if params != "" {
		body["params"] = params
	}
	return body
}

// browsePost performs one /browse POST over the shared player-API transport
// and returns the raw response body. Headers come from GenerateAPIHeaders
// (cookies + SAPISIDHASH when present — the membership tab's auth); the
// visitorData rides both the X-Goog-Visitor-Id header (via ytcfg) and the
// request context. No internal retry: the scanner owns pacing and the sweep
// retries a failed scan next cycle, mirroring chat's single-attempt fetchChat.
func (s *Service) browsePost(ctx context.Context, reqBody map[string]any, visitorData string) ([]byte, error) {
	endpoint := s.browse.endpoint
	if endpoint == "" {
		endpoint = constants.YouTubeURLs.API + "/browse"
	}
	if key := s.PlayerAPI.APIKey(); key != "" {
		endpoint += "?key=" + key
	}

	ytcfg := DefaultYtcfg()
	ytcfg.VisitorData = visitorData
	headers := s.Auth.GenerateAPIHeaders(constants.WebClient, ytcfg)
	headers["Referer"] = constants.YouTubeURLs.Base + "/"

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal browse request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("browse API error: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBrowseResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read browse response: %w", err)
	}
	return body, nil
}

// browseFirstPageEnvelope decodes a page-1 /browse response in one pass:
// responseContext for the per-page visitorData, plus the embedded
// ytInitialTabs tab list (lazy RawMessage tab bodies, reused as-is from
// channel_membership.go).
type browseFirstPageEnvelope struct {
	ResponseContext struct {
		VisitorData string `json:"visitorData"`
	} `json:"responseContext"`
	ytInitialTabs
}

// parseBrowseTabPage extracts items + continuation token from a page-1
// /browse response. hasTab reports whether a usable selected tab was found at
// all (false drives the videos-tab UU fallback; for membership it means "not
// a member" — the same absent-TAB_ID_SPONSORSHIPS signal parseMembershipTab
// keys on — which the caller treats as clean exhaustion, not an error).
func parseBrowseTabPage(body []byte, tab string) (items []MembershipVideo, token, visitorData string, hasTab bool) {
	var env browseFirstPageEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", "", false
	}
	visitorData = env.ResponseContext.VisitorData

	// Deep-parse ONLY the selected tab body (the lazy-decode rationale at
	// ytInitialTabs). The membership tab must be the selected
	// TAB_ID_SPONSORSHIPS tab, exactly like parseMembershipTab; videos and
	// streams take whichever tab the params selected.
	var content json.RawMessage
	for _, t := range env.Contents.TwoColumnBrowseResultsRenderer.Tabs {
		h := t.TabRenderer
		if h == nil {
			h = t.ExpandableTabRenderer
		}
		if h == nil || !h.Selected || len(h.Content) == 0 {
			continue
		}
		if tab == "membership" && h.TabIdentifier != membershipTabIdentifier {
			continue
		}
		content = h.Content
		break
	}
	if len(content) == 0 {
		return nil, "", visitorData, false
	}

	var tree map[string]any
	if err := json.Unmarshal(content, &tree); err != nil {
		return nil, "", visitorData, false
	}
	seen := make(map[string]struct{})
	walkVideoRenderers(tree, seen, &items)
	return items, findContinuationToken(tree), visitorData, true
}

// findContinuationToken walks a decoded tab/continuation tree for the
// canonical token path: continuationItemRenderer.continuationEndpoint.
// continuationCommand.token (yt-dlp _base.py handles further variants —
// button wrappers, continuationItemViewModel — none of which YouTube serves
// on channel tab grids today). A tab page carries at most one trailing
// continuationItemRenderer, so map iteration order is immaterial.
func findContinuationToken(node any) string {
	switch n := node.(type) {
	case map[string]any:
		if cir, ok := n["continuationItemRenderer"].(map[string]any); ok {
			if tok := getDeepStr(cir, "continuationEndpoint", "continuationCommand", "token"); tok != "" {
				return tok
			}
		}
		for _, v := range n {
			if tok := findContinuationToken(v); tok != "" {
				return tok
			}
		}
	case []any:
		for _, v := range n {
			if tok := findContinuationToken(v); tok != "" {
				return tok
			}
		}
	}
	return ""
}

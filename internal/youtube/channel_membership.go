package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// membershipTabIdentifier is the tabRenderer.tabIdentifier for a channel's
// Membership tab. YouTube localizes the visible tab title ("Membership",
// "メンバーシップ", …) so we key off this stable identifier instead. yt-dlp maps
// the same TAB_ID_SPONSORSHIPS token to its "membership" tab; confirmed against
// live channels 2026-07.
const membershipTabIdentifier = "TAB_ID_SPONSORSHIPS"

// ytInitialDataStartRe locates the start of the ytInitialData JSON object on a
// channel/watch page. YouTube emits it as `var ytInitialData = {…}` (and has
// historically also used `window["ytInitialData"] = {…}`); we match either form
// up to the opening brace, then balance-scan from there. A non-greedy regex
// over the whole object under- or over-matches on the megabyte-scale channel
// payload, so brace-scanning is the robust choice.
var ytInitialDataStartRe = regexp.MustCompile(`ytInitialData(?:"\])?\s*=\s*\{`)

// MembershipVideo is a members-only video discovered from a channel's
// /membership tab. Stream status (live/upcoming/vod) is resolved downstream by
// an authenticated probe; Age is a coarse recency estimate used only to merge
// and rank membership items against dated RSS items (the tab exposes no exact
// timestamp — a past item shows only "Streamed N <unit> ago"). Age is 0 for a
// live/upcoming/unrecognized item (treated as "now"), so it always ranks into
// the cap; only a proven past VOD gets a non-zero Age and can be crowded out.
type MembershipVideo struct {
	VideoID string
	Title   string
	Age     time.Duration // ~time since it streamed; 0 = live/upcoming/now
}

// relativeAgeRe matches YouTube's relative published text, e.g. "Streamed 2
// years ago" / "3 weeks ago". Localized to en (we send Accept-Language: en).
// Its PRESENCE marks a past stream/upload; its ABSENCE means the item is live,
// upcoming, or unrecognized — all treated as "now" (see itemAge).
var relativeAgeRe = regexp.MustCompile(`(\d+)\s+(second|minute|hour|day|week|month|year)s?\s+ago`)

// FetchMembershipVideos fetches a channel's /membership tab with the current
// auth cookies and returns the members-only videos listed there.
//
// It returns (nil, nil) — no error — when discovery is simply not possible or
// not applicable: no auth cookies are loaded, or the account is not a member of
// the channel (the /membership tab then falls back to a public tab or a "join"
// upsell that carries no members content, detected by the absence of a selected
// TAB_ID_SPONSORSHIPS tab). Callers treat an empty result as "nothing to do".
//
// Discovery does NOT require the members badge or SAPISIDHASH — a plain
// authenticated GET of the HTML page carries the session, exactly like
// FetchWatchPage. Cookies matter for downloading the stream, which the worker
// already handles; here they only unlock the membership tab listing.
func (s *Service) FetchMembershipVideos(ctx context.Context, channelID string) ([]MembershipVideo, error) {
	if !s.Auth.HasAuthCookies() {
		return nil, nil
	}
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before membership fetch", "error", err)
	}

	url := fmt.Sprintf("%s/channel/%s/membership", constants.YouTubeURLs.Base, channelID)
	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if ch := s.Auth.GetCookieHeader(); ch != "" {
		headers["Cookie"] = ch
	}

	body, err := utils.FetchBody(ctx, url, 20*time.Second, headers)
	if err != nil {
		return nil, fmt.Errorf("fetch membership tab: %w", err)
	}

	// Parse straight off the response bytes — no string(body)/[]byte(raw) copies
	// of the ~1MB payload. json.Unmarshal copies any strings it keeps, so the
	// body is free to be GC'd once this returns.
	videos, hasAccess := parseMembershipTab(body)
	if !hasAccess {
		return nil, nil
	}
	return videos, nil
}

// membershipTabHeader captures only the fields needed to locate the selected
// membership tab. The (large) tab body stays a json.RawMessage so the deep JSON
// walk runs over ONLY the one tab we use — a non-member's home-fallback page is
// never deep-parsed at all. This is the hot path (fetched every cycle for every
// YouTube channel), and lazy-decoding cut allocations ~99% for the common
// non-member case (measured on a real fallback page: 98k → 32 allocs).
type membershipTabHeader struct {
	Selected      bool            `json:"selected"`
	TabIdentifier string          `json:"tabIdentifier"`
	Content       json.RawMessage `json:"content"`
}

// ytInitialTabs is a minimal envelope over ytInitialData: just the channel tab
// list, with each tab's body left as raw bytes for lazy decoding.
type ytInitialTabs struct {
	Contents struct {
		TwoColumnBrowseResultsRenderer struct {
			Tabs []struct {
				TabRenderer           *membershipTabHeader `json:"tabRenderer"`
				ExpandableTabRenderer *membershipTabHeader `json:"expandableTabRenderer"`
			} `json:"tabs"`
		} `json:"twoColumnBrowseResultsRenderer"`
	} `json:"contents"`
}

// parseMembershipTab extracts the members-only video list from a /membership
// page's ytInitialData. The second return value reports whether the account
// actually has membership access: it is true only when the page's SELECTED tab
// is the TAB_ID_SPONSORSHIPS membership tab. For a non-member the page falls
// back to the channel Home tab (or a join upsell with no selected tab), which
// this reports as (nil, false) so the caller ingests zero public videos.
func parseMembershipTab(data []byte) ([]MembershipVideo, bool) {
	raw, ok := extractYtInitialData(data)
	if !ok {
		return nil, false
	}

	// Decode only the tab wrapper; each tab body stays raw so we skip the deep
	// parse of every non-selected tab (and skip it entirely for a non-member).
	var env ytInitialTabs
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}

	var content json.RawMessage
	for _, t := range env.Contents.TwoColumnBrowseResultsRenderer.Tabs {
		h := t.TabRenderer
		if h == nil {
			h = t.ExpandableTabRenderer
		}
		if h != nil && h.Selected && h.TabIdentifier == membershipTabIdentifier {
			content = h.Content
			break
		}
	}
	if len(content) == 0 {
		return nil, false // not a member / membership tab not selected
	}

	// Deep-parse ONLY the selected membership tab body, then walk it for IDs.
	var tab map[string]any
	if err := json.Unmarshal(content, &tab); err != nil {
		return nil, false
	}

	seen := make(map[string]struct{})
	var videos []MembershipVideo
	walkVideoRenderers(tab, seen, &videos)
	return videos, true
}

// walkVideoRenderers recursively collects video IDs (+ best-effort titles) from
// both the current lockupViewModel layout (contentId) and the classic
// videoRenderer/gridVideoRenderer layout (videoId). YouTube A/B-serves both
// shapes, so we handle each. Dedup by video ID keeps a video that appears in
// several shelves from being queued twice.
func walkVideoRenderers(node any, seen map[string]struct{}, out *[]MembershipVideo) {
	switch n := node.(type) {
	case map[string]any:
		if lv, ok := n["lockupViewModel"].(map[string]any); ok {
			if cid := getStr(lv, "contentId"); len(cid) == 11 {
				addVideo(cid, lockupTitle(lv), itemAge(lv), seen, out)
			}
		}
		for _, key := range []string{"videoRenderer", "gridVideoRenderer", "playlistVideoRenderer"} {
			if r, ok := n[key].(map[string]any); ok {
				if vid := getStr(r, "videoId"); len(vid) == 11 {
					addVideo(vid, rendererTitle(r), itemAge(r), seen, out)
				}
			}
		}
		for _, v := range n {
			walkVideoRenderers(v, seen, out)
		}
	case []any:
		for _, v := range n {
			walkVideoRenderers(v, seen, out)
		}
	}
}

func addVideo(id, title string, age time.Duration, seen map[string]struct{}, out *[]MembershipVideo) {
	if _, dup := seen[id]; dup {
		return
	}
	seen[id] = struct{}{}
	*out = append(*out, MembershipVideo{VideoID: id, Title: title, Age: age})
}

// itemAge estimates how long ago a membership item streamed, for recency
// ranking in the merged candidate list. The ONLY thing that pushes an item down
// the ranking is a "Streamed N <unit> ago" text, which marks a PAST stream: it
// is ranked by that age so old VODs sink and get crowded out of the cap.
// Everything else — a live badge, an upcoming stream, or an item with no
// recognizable timestamp — returns 0 ("now"), so it ranks to the top and is
// always probed. Keying on the ABSENCE of a past-time signal (rather than the
// PRESENCE of a live badge) makes catching live/upcoming members streams robust
// to YouTube's frequent badge DOM churn — a live/upcoming item can never sink
// below dated VODs and be dropped from the cap. Scanning the serialized item is
// layout-agnostic (lockup and classic renderers both work).
func itemAge(item map[string]any) time.Duration {
	b, err := json.Marshal(item)
	if err != nil {
		return 0
	}
	s := string(b)
	// Currently live → "now", regardless of any "streaming for N" elapsed text.
	if strings.Contains(s, "THUMBNAIL_OVERLAY_BADGE_STYLE_LIVE") ||
		strings.Contains(s, `"imageName":"LIVE"`) ||
		strings.Contains(s, "BADGE_STYLE_TYPE_LIVE_NOW") {
		return 0
	}
	// A "Streamed N <unit> ago" text marks a PAST stream → rank by that age.
	if m := relativeAgeRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		var unit time.Duration
		switch m[2] {
		case "second":
			unit = time.Second
		case "minute":
			unit = time.Minute
		case "hour":
			unit = time.Hour
		case "day":
			unit = 24 * time.Hour
		case "week":
			unit = 7 * 24 * time.Hour
		case "month":
			unit = 30 * 24 * time.Hour
		case "year":
			unit = 365 * 24 * time.Hour
		}
		return time.Duration(n) * unit
	}
	// No live badge and no past-time text → upcoming or unrecognized → "now",
	// so live/upcoming catching never depends on a single fragile badge marker.
	return 0
}

// lockupTitle reads the title from a lockupViewModel:
// metadata.lockupMetadataViewModel.title.content.
func lockupTitle(lv map[string]any) string {
	return getDeepStr(lv, "metadata", "lockupMetadataViewModel", "title", "content")
}

// rendererTitle reads the title from a classic videoRenderer, handling both the
// title.simpleText and title.runs[].text shapes (falling back to headline).
func rendererTitle(r map[string]any) string {
	title, ok := r["title"].(map[string]any)
	if !ok {
		if title, ok = r["headline"].(map[string]any); !ok {
			return ""
		}
	}
	if s := getStr(title, "simpleText"); s != "" {
		return s
	}
	runs, ok := title["runs"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, run := range runs {
		if rm, ok := run.(map[string]any); ok {
			b.WriteString(getStr(rm, "text"))
		}
	}
	return b.String()
}

// extractYtInitialData pulls the ytInitialData JSON object out of a channel
// (or watch) page via a brace-depth scan that respects string literals. Returns
// a sub-slice of the input (no copy) and true on success. Balancing braces
// (rather than a non-greedy regex) is necessary because the channel payload is
// large and deeply nested. Works on []byte to avoid copying the ~1MB page.
func extractYtInitialData(data []byte) ([]byte, bool) {
	loc := ytInitialDataStartRe.FindIndex(data)
	if loc == nil {
		return nil, false
	}
	start := loc[1] - 1 // index of the opening '{'

	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(data); i++ {
		c := data[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return data[start : i+1], true
			}
		}
	}
	return nil, false
}

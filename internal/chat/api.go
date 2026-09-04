package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// ErrAuthRequired is returned from FetchLiveChat / FetchChatReplay when the
// YouTube Innertube endpoint answers with HTTP 401. This is the canonical
// symptom of expired SAPISID cookies for member-gated chat; the chat
// downloader detects it and aborts rather than burning its consecutive-error
// retry budget on a fundamentally broken credential state (audit chat.md T5).
// Callers that wish to recover should refresh cookies and re-Start the
// chat downloader.
var ErrAuthRequired = errors.New("chat authentication required (HTTP 401)")

const (
	liveChatEndpoint   = "https://www.youtube.com/youtubei/v1/live_chat/get_live_chat"
	replayChatEndpoint = "https://www.youtube.com/youtubei/v1/live_chat/get_live_chat_replay"
	youtubeBase        = "https://www.youtube.com"

	// chatHTTPTimeout caps both watch-page fetches and chat-API polls.
	chatHTTPTimeout = 30 * time.Second

	// maxWatchPageBytes caps the watch page body. YouTube's watch page can
	// approach 3-4 MB on busy streams, so the ceiling is 10 MB to leave
	// headroom for ytInitialData growth without letting a runaway response
	// blow memory (audit reports/chat.md Q5).
	maxWatchPageBytes = 10 << 20

	// maxChatResponseBytes caps a single chat API response. Live chat
	// responses are typically <100 KB; 5 MB is a generous upper bound that
	// catches API drift / pathological replays without being a real ceiling
	// (audit reports/chat.md Q7).
	maxChatResponseBytes = 5 << 20
)

var ytInitialDataRegex = regexp.MustCompile(`(?s)var ytInitialData = ({.+?});</script>`)

// ChatApiResponse contains the parsed response from a chat API call.
type ChatApiResponse struct {
	Messages            []ChatMessage
	NextContinuation    string
	AllChatContinuation string // Unfiltered "Live Chat" token from header (first response only)
	TimeoutMs           int
	IsComplete          bool
}

// ChatAPI handles YouTube live chat API interactions.
type ChatAPI struct {
	apiKey      string
	visitorData string
	// cookieHeader returns the CURRENT Cookie header, re-read on every
	// request. It is a getter and not a captured string because live chat
	// polls this API every ~5 s for hours while the in-process cookie refresh
	// rotates Google's cookies roughly every 30 minutes; a snapshot taken at
	// construction goes stale mid-job, 401s, and ends chat capture for good.
	// nil-safe — see generateAuth below, whose shape this mirrors.
	cookieHeader func() string
	generateAuth func() string // Returns Authorization header (SAPISIDHASH) for authenticated requests
	client       *http.Client
	// clientContext is the constant Innertube "context" object (client info +
	// visitorData), built once and reused for every poll. Live chat polls the
	// same endpoint every ~5s for hours; rebuilding + re-marshaling this nested
	// map each time was needless per-poll allocation. Read-only after
	// construction, so concurrent json.Marshal reads are safe.
	clientContext map[string]any

	// Logger is an optional debug-level diagnostic sink for API drift
	// signals (unexpected field shapes, parse failures). nil-safe.
	Logger interface {
		Debug(msg string, args ...any)
	}
}

// logDebug routes a debug-level diagnostic through the optional Logger.
// No-op when Logger is nil.
func (api *ChatAPI) logDebug(msg string, args ...any) {
	if api.Logger != nil {
		api.Logger.Debug(msg, args...)
	}
}

// NewChatAPI creates a new chat API client.
//
// Live chat polls every ~5 s for hours at a time. Backed by the shared
// httpx transport (MaxIdleConnsPerHost=8) so keep-alive amortises the
// handshake across the per-poll cadence. Audit chat.md R3.
//
// cookieHeader is a getter, not a value: it is called at request time so the
// client presents whatever cookies the jar holds now, not the ones it started
// with. nil is allowed and means "send no Cookie header".
func NewChatAPI(apiKey, visitorData string, cookieHeader func() string) *ChatAPI {
	client := map[string]any{
		"clientName":    "WEB",
		"clientVersion": constants.WebClient.ClientVersion,
		"hl":            "en",
		"gl":            "US",
	}
	if visitorData != "" {
		client["visitorData"] = visitorData
	}
	return &ChatAPI{
		apiKey:        apiKey,
		visitorData:   visitorData,
		cookieHeader:  cookieHeader,
		client:        httpx.Client(chatHTTPTimeout),
		clientContext: map[string]any{"client": client},
	}
}

// FetchFreshContinuation fetches a chat continuation token from the watch page.
func (api *ChatAPI) FetchFreshContinuation(ctx context.Context, videoID string) (continuation string, isReplay bool, err error) {
	url := fmt.Sprintf("%s/watch?v=%s", youtubeBase, videoID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	// Force English-language watch pages so the EU consent gate hits a
	// known-shape variant (we detect it via redirect URL below) and so
	// ytInitialData field names stay stable. DNT is best-effort hygiene
	// (audit chat.md C14).
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("DNT", "1")
	if api.cookieHeader != nil {
		if ch := api.cookieHeader(); ch != "" {
			req.Header.Set("Cookie", ch)
		}
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	// Detect EU/consent-wall redirect — the response URL switches to
	// consent.youtube.com / consent.google.com when YouTube wants the
	// viewer to accept tracking before serving the watch page. The
	// returned HTML has no ytInitialData so extraction silently fails;
	// surface the cause distinctly so operators know to ship CONSENT
	// cookies (audit chat.md C14).
	if resp.Request != nil && resp.Request.URL != nil {
		host := resp.Request.URL.Host
		if strings.HasPrefix(host, "consent.") {
			io.Copy(io.Discard, resp.Body)
			return "", false, fmt.Errorf("watch page redirected to consent wall (%s); supply CONSENT cookies", host)
		}
	}

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", false, fmt.Errorf("watch page returned status %d", resp.StatusCode)
	}

	// Watch pages on popular streams can exceed 5 MB once ytInitialData,
	// ytInitialPlayerResponse and all sidebar chrome are rendered. Chat API
	// responses stay at 5 MB; only the watch-page fetch gets the 10 MB cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWatchPageBytes))
	if err != nil {
		return "", false, err
	}

	return ExtractChatContinuation(string(body))
}

// FetchLiveChat fetches live chat messages using the given continuation.
func (api *ChatAPI) FetchLiveChat(ctx context.Context, continuation string) (*ChatApiResponse, error) {
	return api.fetchChat(ctx, liveChatEndpoint, continuation)
}

// FetchChatReplay fetches chat replay messages using the given continuation.
func (api *ChatAPI) FetchChatReplay(ctx context.Context, continuation string) (*ChatApiResponse, error) {
	return api.fetchChat(ctx, replayChatEndpoint, continuation)
}

func (api *ChatAPI) fetchChat(ctx context.Context, endpoint, continuation string) (*ChatApiResponse, error) {
	reqBody := map[string]any{
		"context":      api.clientContext,
		"continuation": continuation,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := endpoint
	if api.apiKey != "" {
		url += "?key=" + api.apiKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Origin", youtubeBase)
	req.Header.Set("Referer", youtubeBase)
	if api.cookieHeader != nil {
		if ch := api.cookieHeader(); ch != "" {
			req.Header.Set("Cookie", ch)
		}
	}
	// Add SAPISIDHASH authorization for member-gated chat
	if api.generateAuth != nil {
		if auth := api.generateAuth(); auth != "" {
			req.Header.Set("Authorization", auth)
			req.Header.Set("X-Origin", youtubeBase)
		}
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: status %d", ErrAuthRequired, resp.StatusCode)
		}
		return nil, fmt.Errorf("chat API returned status %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes))
	if err != nil {
		return nil, err
	}

	var data map[string]any
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}

	return api.parseResponse(data)
}

// ExtractChatContinuation extracts a chat continuation token from watch page HTML.
func ExtractChatContinuation(html string) (string, bool, error) {
	// FindStringSubmatch avoids copying the entire (up-to-10 MB) watch page into
	// a []byte just to run the regex; only the small captured group is copied
	// to []byte below for json.Unmarshal.
	m := ytInitialDataRegex.FindStringSubmatch(html)
	if m == nil {
		return "", false, fmt.Errorf("ytInitialData not found")
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(m[1]), &data); err != nil {
		return "", false, fmt.Errorf("parse ytInitialData: %w", err)
	}

	// Navigate: contents.twoColumnWatchNextResults.conversationBar.liveChatRenderer
	contents, _ := data["contents"].(map[string]any)
	twoCol, _ := contents["twoColumnWatchNextResults"].(map[string]any)
	convBar, _ := twoCol["conversationBar"].(map[string]any)
	chatRenderer, _ := convBar["liveChatRenderer"].(map[string]any)

	if chatRenderer == nil {
		return "", false, fmt.Errorf("no liveChatRenderer found")
	}

	isReplay, _ := chatRenderer["isReplay"].(bool)

	// Extract continuation — check ALL items, not just first
	conts, _ := chatRenderer["continuations"].([]any)
	if len(conts) == 0 {
		return "", false, fmt.Errorf("no continuations found")
	}

	for _, cont := range conts {
		contMap, _ := cont.(map[string]any)
		if contMap == nil {
			continue
		}
		for _, key := range []string{
			"reloadContinuationData",
			"invalidationContinuationData",
			"timedContinuationData",
			"liveChatReplayContinuationData",
		} {
			if contData, ok := contMap[key].(map[string]any); ok {
				if token, ok := contData["continuation"].(string); ok && token != "" {
					return token, isReplay, nil
				}
			}
		}
	}

	return "", false, fmt.Errorf("no continuation token found")
}

// parseResponse parses the chat API response into structured data.
func (api *ChatAPI) parseResponse(data map[string]any) (*ChatApiResponse, error) {
	result := &ChatApiResponse{
		TimeoutMs: -1, // -1 = not set; let caller decide default based on isReplay
	}

	contContents, _ := data["continuationContents"].(map[string]any)
	liveChatCont, _ := contContents["liveChatContinuation"].(map[string]any)
	if liveChatCont == nil {
		result.IsComplete = true
		result.TimeoutMs = 0
		return result, nil
	}

	result.NextContinuation, result.TimeoutMs = extractNextContinuation(liveChatCont, result.TimeoutMs)
	if result.NextContinuation == "" {
		result.IsComplete = true
	}

	actions, _ := liveChatCont["actions"].([]any)
	for _, action := range actions {
		actionMap, _ := action.(map[string]any)
		if actionMap == nil {
			continue
		}
		if msg := api.parseAction(actionMap); msg != nil {
			result.Messages = append(result.Messages, *msg)
		}
	}

	if header, _ := liveChatCont["header"].(map[string]any); header != nil {
		result.AllChatContinuation = extractAllChatContinuation(header)
	}

	return result, nil
}

// extractNextContinuation walks the continuations array and returns the first
// token found plus its timeoutMs hint. Returns ("", defaultTimeoutMs) when
// no token is found — the caller uses the empty token to set IsComplete.
// YouTube sometimes ships multiple continuation entries in the array; we
// stop at the first one carrying a non-empty continuation string.
func extractNextContinuation(liveChatCont map[string]any, defaultTimeoutMs int) (token string, timeoutMs int) {
	timeoutMs = defaultTimeoutMs
	conts, _ := liveChatCont["continuations"].([]any)
	for _, cont := range conts {
		contMap, _ := cont.(map[string]any)
		if contMap == nil {
			continue
		}
		for _, key := range []string{
			"timedContinuationData",
			"invalidationContinuationData",
			"liveChatReplayContinuationData",
		} {
			contData, ok := contMap[key].(map[string]any)
			if !ok {
				continue
			}
			if t, ok := contData["continuation"].(string); ok && t != "" {
				token = t
			}
			if ms, ok := contData["timeoutMs"].(float64); ok {
				timeoutMs = int(ms)
			}
			break
		}
		if token != "" {
			break
		}
	}
	return
}

// parseAction parses a single chat action into a ChatMessage. Handles the
// replay wrapper unwrap, replay-offset extraction, renderer-type selection,
// superchat / sticker / membership annotation. Returns nil when the action
// is not a chat message we recognise (missing renderer, malformed replay
// wrapper, non-chat action type).
func (api *ChatAPI) parseAction(actionMap map[string]any) *ChatMessage {
	var replayOffsetMs int64
	hasReplayOffset := false
	if replayAction, ok := actionMap["replayChatItemAction"].(map[string]any); ok {
		innerActions, _ := replayAction["actions"].([]any)
		if len(innerActions) > 0 {
			inner, _ := innerActions[0].(map[string]any)
			if inner == nil {
				return nil
			}
			actionMap = inner
		}
		replayOffsetMs, hasReplayOffset = api.extractReplayOffset(replayAction)
	}

	addAction, _ := actionMap["addChatItemAction"].(map[string]any)
	if addAction == nil {
		return nil
	}
	item, _ := addAction["item"].(map[string]any)
	if item == nil {
		return nil
	}

	renderer := selectRenderer(item)
	if renderer == nil {
		return nil
	}

	msg := api.parseMessageRenderer(renderer)
	if msg == nil {
		return nil
	}

	if paid, ok := item["liveChatPaidMessageRenderer"].(map[string]any); ok {
		msg.Superchat = api.parseSuperChatInfo(paid)
	} else if sticker, ok := item["liveChatPaidStickerRenderer"].(map[string]any); ok {
		msg.Superchat = api.parseSuperChatInfo(sticker)
	}
	if _, ok := item["liveChatMembershipItemRenderer"]; ok {
		msg.IsMembership = true
	}
	if hasReplayOffset {
		if replayOffsetMs == 0 {
			// Pre-stream replay messages arrive with offset 0; the relative
			// time survives only in timestampText (review 2026-09-03, N-F2).
			if neg, ok := parseNegativeTimestampText(msg.TimestampText); ok {
				replayOffsetMs = neg
			}
		}
		// Negative offsets are legitimate for pre-stream waiting-room chat.
		msg.OffsetMs = replayOffsetMs
		msg.HasOffset = true
	}
	return msg
}

// extractReplayOffset pulls videoOffsetTimeMsec out of a replayChatItemAction.
// Accepts both the typical string form ("12345") and the raw-number form
// (JSON float64) that some responses ship (audit chat.md C11). Returns
// (0, false) when the field is missing or unparseable; the caller falls back
// to live-mode offset computation.
func (api *ChatAPI) extractReplayOffset(replayAction map[string]any) (int64, bool) {
	raw, present := replayAction["videoOffsetTimeMsec"]
	if !present {
		return 0, false
	}
	switch v := raw.(type) {
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return parsed, true
		}
		api.logDebug("chat: videoOffsetTimeMsec string parse failed", "value", v, "err", err)
	case float64:
		return int64(v), true
	default:
		api.logDebug("chat: videoOffsetTimeMsec unexpected type", "type", fmt.Sprintf("%T", raw))
	}
	return 0, false
}

// parseNegativeTimestampText parses YouTube's relative replay timestamp text
// for a PRE-STREAM message ("-1:23", "-1:02:03") into signed milliseconds.
// YouTube zeroes videoOffsetTimeMsec for messages sent before the stream
// started and keeps the real time only here. Returns (0, false) for anything
// that is not a leading-minus M:SS / H:MM:SS.
func parseNegativeTimestampText(s string) (int64, bool) {
	if !strings.HasPrefix(s, "-") {
		return 0, false
	}
	parts := strings.Split(s[1:], ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total int64
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + int64(n)
	}
	return -total * 1000, true
}

// selectRenderer picks the first recognised renderer type from the action
// item. Returns nil for unknown renderer types (silently skipped — YouTube
// adds new renderer kinds periodically; unrecognised ones are not fatal).
func selectRenderer(item map[string]any) map[string]any {
	for _, key := range []string{
		"liveChatTextMessageRenderer",
		"liveChatPaidMessageRenderer",
		"liveChatPaidStickerRenderer",
		"liveChatMembershipItemRenderer",
	} {
		if r, ok := item[key].(map[string]any); ok {
			return r
		}
	}
	return nil
}

// extractAllChatContinuation pulls the unfiltered "Live Chat" continuation
// token out of the liveChatHeaderRenderer.viewSelector sub-menu. YouTube
// defaults the chat view to "Top Chat" (filtered); subMenuItems[1] carries
// the continuation that upgrades to the unfiltered view. Returns "" if any
// step of the navigation is missing — callers stay on Top Chat and retry on
// a subsequent poll (audit chat.md R1).
func extractAllChatContinuation(header map[string]any) string {
	lchRenderer, _ := header["liveChatHeaderRenderer"].(map[string]any)
	viewSelector, _ := lchRenderer["viewSelector"].(map[string]any)
	sortFilter, _ := viewSelector["sortFilterSubMenuRenderer"].(map[string]any)
	subMenuItems, _ := sortFilter["subMenuItems"].([]any)
	if len(subMenuItems) <= 1 {
		return ""
	}
	item, _ := subMenuItems[1].(map[string]any)
	cont, _ := item["continuation"].(map[string]any)
	reload, _ := cont["reloadContinuationData"].(map[string]any)
	if token, ok := reload["continuation"].(string); ok && token != "" {
		return token
	}
	return ""
}

func (api *ChatAPI) parseMessageRenderer(renderer map[string]any) *ChatMessage {
	msg := &ChatMessage{}

	msg.ID, _ = renderer["id"].(string)
	if msg.ID == "" {
		msg.ID = generateMessageID()
	}

	msg.TimestampUsec, _ = renderer["timestampUsec"].(string)
	if text, ok := renderer["timestampText"].(map[string]any); ok {
		msg.TimestampText, _ = text["simpleText"].(string)
	}
	if msg.TimestampText == "" && msg.TimestampUsec != "" {
		msg.TimestampText = api.formatTimestamp(msg.TimestampUsec)
	}

	// Author — check both simpleText and runs
	if authorName, ok := renderer["authorName"].(map[string]any); ok {
		msg.AuthorName, _ = authorName["simpleText"].(string)
		if msg.AuthorName == "" {
			if runs, ok := authorName["runs"].([]any); ok && len(runs) > 0 {
				if run, ok := runs[0].(map[string]any); ok {
					msg.AuthorName, _ = run["text"].(string)
				}
			}
		}
	}
	if msg.AuthorName == "" {
		msg.AuthorName = "Unknown"
	}
	if extChannel, ok := renderer["authorExternalChannelId"].(string); ok {
		msg.AuthorChannelID = extChannel
	}

	// Badges
	msg.AuthorBadges = extractBadges(renderer)

	// Message content
	if message, ok := renderer["message"].(map[string]any); ok {
		msg.Message = parseMessageRuns(message)
	}

	return msg
}

func (api *ChatAPI) parseSuperChatInfo(paidRenderer map[string]any) *SuperchatInfo {
	sc := &SuperchatInfo{}

	if purchase, ok := paidRenderer["purchaseAmountText"].(map[string]any); ok {
		amountText, _ := purchase["simpleText"].(string)
		sc.Amount = amountText
		sc.Currency = extractCurrency(amountText)
	}

	// Use headerBackgroundColor (not bodyBackgroundColor).
	// ARGB color values are 32-bit unsigned ints; convert via uint32 so
	// the lookup key matches superchatTierColors' natural type.
	if bgColor, ok := paidRenderer["headerBackgroundColor"].(float64); ok {
		colorVal := uint32(bgColor)
		if tier, ok := superchatTierColors[colorVal]; ok {
			sc.Tier = tier.tier
			sc.Color = tier.color
		} else {
			// Unknown bgColor — log at debug so tier-table drift is visible
			// without polluting the error channel.
			api.logDebug("chat: unknown superchat headerBackgroundColor", "color", colorVal)
			sc.Tier = 1
			sc.Color = "blue"
		}
	}

	return sc
}

func parseMessageRuns(message map[string]any) []MessagePart {
	runs, _ := message["runs"].([]any)
	var parts []MessagePart

	for _, run := range runs {
		runMap, _ := run.(map[string]any)
		if runMap == nil {
			continue
		}

		if text, ok := runMap["text"].(string); ok {
			part := MessagePart{Type: "text", Text: text}
			// navigationEndpoint on a text run carries a hyperlink target
			// (chat mentions, pasted URLs, referenced channels). The
			// urlEndpoint form has the raw target; commandMetadata has
			// YouTube's redirect-wrapped variant — prefer the direct one
			// (audit chat.md E5).
			if nav, ok := runMap["navigationEndpoint"].(map[string]any); ok {
				part.URL = extractNavURL(nav)
			}
			if bold, ok := runMap["bold"].(bool); ok && bold {
				part.Bold = true
			}
			if italic, ok := runMap["italics"].(bool); ok && italic {
				part.Italic = true
			}
			parts = append(parts, part)
		} else if emoji, ok := runMap["emoji"].(map[string]any); ok {
			part := MessagePart{Type: "emoji"}
			if emojiID, ok := emoji["emojiId"].(string); ok {
				part.EmojiID = emojiID
			}
			// Fallback to shortcuts
			if part.EmojiID == "" {
				if shortcuts, ok := emoji["shortcuts"].([]any); ok && len(shortcuts) > 0 {
					if s, ok := shortcuts[0].(string); ok {
						part.EmojiID = s
					}
				}
			}
			if image, ok := emoji["image"].(map[string]any); ok {
				if thumbs, ok := image["thumbnails"].([]any); ok && len(thumbs) > 0 {
					// Pick the largest thumbnail (by width*height, then width,
					// then height) rather than thumbs[0] which is usually the
					// smallest.
					var bestURL string
					var bestScore int64
					for _, t := range thumbs {
						tm, ok := t.(map[string]any)
						if !ok {
							continue
						}
						url, _ := tm["url"].(string)
						if url == "" {
							continue
						}
						w, _ := tm["width"].(float64)
						h, _ := tm["height"].(float64)
						score := int64(w) * int64(h)
						if score == 0 {
							// Fallback when dimensions are missing — prefer
							// whichever has dimensions, else just the last one.
							score = int64(w) + int64(h)
						}
						if bestURL == "" || score > bestScore {
							bestURL = url
							bestScore = score
						}
					}
					part.EmojiURL = bestURL
				}
			}
			if v, ok := emoji["isCustomEmoji"].(bool); ok {
				part.IsCustomEmoji = v
			}
			parts = append(parts, part)
		}
	}

	return parts
}

// extractNavURL pulls a hyperlink target out of a YouTube navigationEndpoint.
// Prefers urlEndpoint.url (direct target) and falls back to
// commandMetadata.webCommandMetadata.url (YouTube's redirect-wrapped form).
// Returns "" if neither shape is present.
func extractNavURL(nav map[string]any) string {
	if ue, ok := nav["urlEndpoint"].(map[string]any); ok {
		if url, ok := ue["url"].(string); ok && url != "" {
			return url
		}
	}
	if cm, ok := nav["commandMetadata"].(map[string]any); ok {
		if wcm, ok := cm["webCommandMetadata"].(map[string]any); ok {
			if url, ok := wcm["url"].(string); ok && url != "" {
				return url
			}
		}
	}
	return ""
}

func extractBadges(renderer map[string]any) []string {
	var badges []string
	badgeArray, _ := renderer["authorBadges"].([]any)
	for _, badge := range badgeArray {
		badgeMap, _ := badge.(map[string]any)
		if badgeMap == nil {
			continue
		}
		badgeRenderer, ok := badgeMap["liveChatAuthorBadgeRenderer"].(map[string]any)
		if !ok {
			badgeRenderer = badgeMap
		}

		if icon, ok := badgeRenderer["icon"].(map[string]any); ok {
			if iconType, ok := icon["iconType"].(string); ok {
				switch strings.ToUpper(iconType) {
				case "OWNER":
					badges = append(badges, "owner")
				case "MODERATOR":
					badges = append(badges, "moderator")
				case "VERIFIED":
					badges = append(badges, "verified")
				case "MEMBER":
					// Direct MEMBER iconType detection — more reliable than
					// tooltip matching, which fails on localized clients.
					if !slices.Contains(badges, "member") {
						badges = append(badges, "member")
					}
				default:
					// Unknown iconType — store the raw (lowercased) value so
					// the UI can render new YouTube badge types without a code
					// change here.
					lower := strings.ToLower(iconType)
					if lower != "" && !slices.Contains(badges, lower) {
						badges = append(badges, lower)
					}
				}
			}
		}

		// Check tooltip for member badge (localization-dependent — English only,
		// but kept as a secondary signal to catch older responses without iconType)
		if tooltip, ok := badgeRenderer["tooltip"].(string); ok {
			if strings.Contains(strings.ToLower(tooltip), "member") {
				if !slices.Contains(badges, "member") {
					badges = append(badges, "member")
				}
			}
		}

		// Custom thumbnail = membership badge
		if _, ok := badgeRenderer["customThumbnail"]; ok {
			if !slices.Contains(badges, "member") {
				badges = append(badges, "member")
			}
		}
	}
	return badges
}

// extractCurrency pulls the currency code out of a YouTube SuperChat amount
// string. Handles the common prefix-form ("$5.00", "€2.00") plus several
// suffix-form locales ("5,00 €", Scandinavian/EU). Audit reports/chat.md
// C16/Q15 — the prior implementation only scanned left-to-right and returned
// "UNKNOWN" for any string that started with a digit.
func extractCurrency(amountText string) string {
	if strings.HasPrefix(amountText, "$") {
		return "USD"
	}
	if strings.HasPrefix(amountText, "€") {
		return "EUR"
	}
	if strings.HasPrefix(amountText, "£") {
		return "GBP"
	}
	if strings.HasPrefix(amountText, "¥") {
		return "JPY"
	}
	if strings.Contains(amountText, "CA$") {
		return "CAD"
	}
	if strings.Contains(amountText, "A$") {
		return "AUD"
	}
	// Prefix form: leading non-digit run is the currency.
	for i, c := range amountText {
		if c >= '0' && c <= '9' || c == '.' || c == ',' || c == ' ' {
			if i > 0 {
				return strings.TrimSpace(amountText[:i])
			}
			break // Falls through to suffix scan below.
		}
	}
	// Suffix form: trailing non-digit run after the last digit/separator.
	for i := len(amountText) - 1; i >= 0; i-- {
		c := rune(amountText[i])
		if c >= '0' && c <= '9' || c == '.' || c == ',' {
			if i < len(amountText)-1 {
				return strings.TrimSpace(amountText[i+1:])
			}
			break
		}
	}
	return "UNKNOWN"
}

func (api *ChatAPI) formatTimestamp(usec string) string {
	ts, err := strconv.ParseInt(usec, 10, 64)
	if err != nil {
		api.logDebug("chat: formatTimestamp parse failed", "value", usec, "err", err)
		return "0:00:00"
	}
	if ts == 0 {
		return "0:00:00"
	}
	t := time.UnixMilli(ts / 1000)
	return t.UTC().Format("15:04:05")
}

// generateMessageID synthesises a unique-enough ID for chat messages whose
// renderer omitted "id" (rare — usually system messages). Uses math/rand/v2
// whose top-level funcs are concurrency-safe (v1 globals were not — audit
// chat.md C10/U2).
func generateMessageID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var suffix [6]byte
	for i := range suffix {
		suffix[i] = chars[rand.IntN(len(chars))]
	}
	return fmt.Sprintf("gen-%d-%s", time.Now().UnixMicro(), string(suffix[:]))
}

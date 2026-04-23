package chat

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const (
	liveChatEndpoint   = "https://www.youtube.com/youtubei/v1/live_chat/get_live_chat"
	replayChatEndpoint = "https://www.youtube.com/youtubei/v1/live_chat/get_live_chat_replay"
	youtubeBase        = "https://www.youtube.com"
	chatHTTPTimeout = 30 * time.Second // Matches TS fetchWithTimeout default (30s)
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
	apiKey       string
	visitorData  string
	cookieHeader string
	generateAuth func() string // Returns Authorization header (SAPISIDHASH) for authenticated requests
	client       *http.Client

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
func NewChatAPI(apiKey, visitorData, cookieHeader string) *ChatAPI {
	return &ChatAPI{
		apiKey:       apiKey,
		visitorData:  visitorData,
		cookieHeader: cookieHeader,
		client: &http.Client{
			Timeout: chatHTTPTimeout,
		},
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
	if api.cookieHeader != "" {
		req.Header.Set("Cookie", api.cookieHeader)
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", false, fmt.Errorf("watch page returned status %d", resp.StatusCode)
	}

	// Watch pages on popular streams can exceed 5 MB once ytInitialData,
	// ytInitialPlayerResponse and all sidebar chrome are rendered. Chat API
	// responses stay at 5 MB; only the watch-page fetch gets the 10 MB cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
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
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB",
				"clientVersion": constants.WebClient.ClientVersion,
				"hl":            "en",
				"gl":            "US",
			},
		},
		"continuation": continuation,
	}

	if api.visitorData != "" {
		ctxMap := reqBody["context"].(map[string]any)
		client := ctxMap["client"].(map[string]any)
		client["visitorData"] = api.visitorData
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
	if api.cookieHeader != "" {
		req.Header.Set("Cookie", api.cookieHeader)
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
		return nil, fmt.Errorf("chat API returned status %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
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
	m := ytInitialDataRegex.FindSubmatch([]byte(html))
	if m == nil {
		return "", false, fmt.Errorf("ytInitialData not found")
	}

	var data map[string]any
	if err := json.Unmarshal(m[1], &data); err != nil {
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

	// Extract continuation — check ALL items, not just first
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
			if contData, ok := contMap[key].(map[string]any); ok {
				if token, ok := contData["continuation"].(string); ok && token != "" {
					result.NextContinuation = token
				}
				if timeout, ok := contData["timeoutMs"].(float64); ok {
					result.TimeoutMs = int(timeout)
				}
				break
			}
		}
		if result.NextContinuation != "" {
			break
		}
	}

	if result.NextContinuation == "" {
		result.IsComplete = true
	}

	// Parse actions
	actions, _ := liveChatCont["actions"].([]any)
	for _, action := range actions {
		actionMap, _ := action.(map[string]any)
		if actionMap == nil {
			continue
		}

		// Handle replay wrapper
		var replayOffsetMs int64
		hasReplayOffset := false
		if replayAction, ok := actionMap["replayChatItemAction"].(map[string]any); ok {
			innerActions, _ := replayAction["actions"].([]any)
			if len(innerActions) > 0 {
				actionMap, _ = innerActions[0].(map[string]any)
				if actionMap == nil {
					continue
				}
			}
			// videoOffsetTimeMsec is usually a string (e.g. "12345") but some
			// responses ship it as a raw JSON number — accept both.
			if raw, present := replayAction["videoOffsetTimeMsec"]; present {
				switch v := raw.(type) {
				case string:
					parsed, err := strconv.ParseInt(v, 10, 64)
					if err == nil {
						replayOffsetMs = parsed
						hasReplayOffset = true
					} else {
						api.logDebug("chat: videoOffsetTimeMsec string parse failed", "value", v, "err", err)
					}
				case float64:
					replayOffsetMs = int64(v)
					hasReplayOffset = true
				default:
					api.logDebug("chat: videoOffsetTimeMsec unexpected type", "type", fmt.Sprintf("%T", raw))
				}
			}
		}

		// Find message renderer
		addAction, _ := actionMap["addChatItemAction"].(map[string]any)
		if addAction == nil {
			continue
		}
		item, _ := addAction["item"].(map[string]any)
		if item == nil {
			continue
		}

		// Handle all renderer types (including super stickers)
		var renderer map[string]any
		if r, ok := item["liveChatTextMessageRenderer"].(map[string]any); ok {
			renderer = r
		} else if r, ok := item["liveChatPaidMessageRenderer"].(map[string]any); ok {
			renderer = r
		} else if r, ok := item["liveChatPaidStickerRenderer"].(map[string]any); ok {
			renderer = r
		} else if r, ok := item["liveChatMembershipItemRenderer"].(map[string]any); ok {
			renderer = r
		}

		if renderer == nil {
			continue
		}

		msg := api.parseMessageRenderer(renderer)
		if msg == nil {
			continue
		}

		// Check for superchat (paid message or paid sticker)
		if paidRenderer, ok := item["liveChatPaidMessageRenderer"].(map[string]any); ok {
			msg.Superchat = api.parseSuperChatInfo(paidRenderer)
		} else if stickerRenderer, ok := item["liveChatPaidStickerRenderer"].(map[string]any); ok {
			msg.Superchat = api.parseSuperChatInfo(stickerRenderer)
		}

		// Check membership
		if _, ok := item["liveChatMembershipItemRenderer"]; ok {
			msg.IsMembership = true
		}

		// Set replay offset (may be negative for pre-stream waiting-room chat)
		if hasReplayOffset {
			msg.OffsetMs = replayOffsetMs
			msg.HasOffset = true
		}

		result.Messages = append(result.Messages, *msg)
	}

	// Extract unfiltered "Live Chat" continuation from header (available on first response).
	// YouTube defaults to "Top Chat" (filtered); index [1] is the unfiltered "Live Chat" view.
	if header, _ := liveChatCont["header"].(map[string]any); header != nil {
		lchRenderer, _ := header["liveChatHeaderRenderer"].(map[string]any)
		viewSelector, _ := lchRenderer["viewSelector"].(map[string]any)
		sortFilter, _ := viewSelector["sortFilterSubMenuRenderer"].(map[string]any)
		subMenuItems, _ := sortFilter["subMenuItems"].([]any)
		if len(subMenuItems) > 1 {
			item, _ := subMenuItems[1].(map[string]any)
			cont, _ := item["continuation"].(map[string]any)
			reload, _ := cont["reloadContinuationData"].(map[string]any)
			if token, ok := reload["continuation"].(string); ok && token != "" {
				result.AllChatContinuation = token
			}
		}
	}

	return result, nil
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
			parts = append(parts, MessagePart{Type: "text", Text: text})
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
	// Return the first non-digit character(s) as currency
	for i, c := range amountText {
		if c >= '0' && c <= '9' || c == '.' || c == ',' || c == ' ' {
			if i > 0 {
				return strings.TrimSpace(amountText[:i])
			}
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

func generateMessageID() string {
	return fmt.Sprintf("gen-%d-%s", time.Now().UnixMicro(), randomAlphaNum(6))
}

// randomAlphaNum uses math/rand/v2 whose per-package helpers are safe for
// concurrent calls (v1's globals were not, producing a data race when
// multiple chat downloaders running in parallel touched generateMessageID
// simultaneously — audit chat.md C10).
func randomAlphaNum(n int) string {
	// math/rand/v2 top-level funcs are concurrency-safe; see go.dev/ref/mod.
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}


package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	discordTimeout = 15 * time.Second
	// discordMaxAttempts bounds total delivery attempts (first try +
	// retries) across transport errors, Discord 5xx, and 429 responses.
	discordMaxAttempts = 3
	// discordRetryAfterCap rejects absurd 429 Retry-After values — and bounds
	// the pre-emptive empty-bucket sleep — rather than parking a target's
	// whole queue into next week.
	discordRetryAfterCap = 30 * time.Second
	// discordMaxSleepTotal caps CUMULATIVE inter-attempt sleep, bounding one
	// notification's worst-case hold on its target's queue at ~3x15s requests
	// + 30s sleep = ~75s. Two large Retry-After waits would exceed it — a
	// webhook still rate-limited after one honored wait gives up instead.
	//
	// The PRE-EMPTIVE empty-bucket wait (waitForBucket, ≤ discordRetryAfterCap
	// per attempt) is deliberately outside this budget, so the true worst-case
	// hold is ~75s plus those waits. Counting them here would let one
	// legitimate 30s bucket wait forfeit the retries a following 5xx needs —
	// the budget exists to stop a wedged webhook from parking a queue, and a
	// bucket wait is the opposite: the sender doing what Discord asked.
	discordMaxSleepTotal = 30 * time.Second
	// shutdownBucketWaitCap bounds how long a SINGLE-attempt send (SendOnce —
	// in practice the shutdown path) will wait out a known-empty rate bucket.
	// The process force-exits 10s into shutdown, so honouring a window a full
	// discordRetryAfterCap away would lose the embed to os.Exit AND starve
	// every item behind it in that target's FIFO queue. Past this cap SendOnce
	// posts immediately and lets the 429 drop the item: the same outcome as
	// not waiting, minus the seconds spent.
	shutdownBucketWaitCap = 2 * time.Second
	// bucketSkewPad is added to every empty-bucket deadline learned from
	// X-RateLimit-Reset-After. The header is millisecond precision, so a value
	// rounded to NEAREST lets a sender that wakes exactly on it arrive up to
	// half a millisecond before the server's window actually closes — and that
	// request 429s. Measured on a fake Discord that rounds to nearest: 1-3
	// such 429s per 400 requests without the pad, zero with it. Discord's own
	// client libraries apply the same small offset for the same reason. The
	// cost is 50ms of extra latency on a send that was already waiting out a
	// window.
	bucketSkewPad = 50 * time.Millisecond
	// discordErrBodyBytes bounds how much of a rejected webhook's response
	// body is quoted back in the error. Discord's 4xx bodies are small JSON
	// objects naming what it refused ({"message": "Unknown Webhook", "code":
	// 10015}); without them an operator sees only "discord webhook returned
	// 400" and has nothing to act on. An error PAGE, though, can be
	// arbitrarily large, and this error reaches log files and SendTest's HTTP
	// response.
	discordErrBodyBytes = 256
	// discordMessageBodyBytes bounds the ?wait=true response read. A webhook
	// message object with one embed is a few KB; 64 KiB is slack, and the only
	// field parsed out of it is "id".
	discordMessageBodyBytes = 64 << 10
)

// discordRetryBackoff is the inter-attempt sleep schedule for transport
// errors and 5xx responses (429 uses the server's Retry-After instead).
// Package var so tests can shrink it.
var discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{2 * time.Second, 5 * time.Second}

// discordHTTPClient sends Discord webhook requests via the shared
// httpx transport. Per-request timeout is enforced by the context; the
// client-level Timeout (2 * discordTimeout = 30s) is a belt-and-
// suspenders cap in case a retry/redirect chain slips past the
// request context.
var discordHTTPClient = httpx.Client(2 * discordTimeout)

// DiscordWebhook sends notifications via Discord webhook.
//
// One instance per target, held by that target's queue (manager.go), so the
// bucket state below is learned once and reused for every embed to that
// webhook — including across a config hot-reload, because applyTargets keeps a
// surviving target's queue and therefore this pointer.
type DiscordWebhook struct {
	URL string

	// bucketMu guards bucketRefillsAt. One sender goroutine per target means
	// there is no contention in production; the mutex is for SendTest (which
	// builds its own instance) and for the race detector.
	bucketMu sync.Mutex
	// bucketRefillsAt is when this webhook's rate bucket next has room, set
	// from an X-RateLimit-Remaining: 0 response. Zero means "not known empty".
	bucketRefillsAt time.Time
}

type discordPayload struct {
	// Content is the only place a mention can live: per Discord API docs
	// (resources/message.mdx) allowed_mentions governs "mentions in the
	// message content, or components", so an embed can never ping anyone.
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds"`
	// AllowedMentions is the EXPORTED notifications.AllowedMentions
	// (manager.go), not a payload-private twin: Arc N2b resolves one per
	// (target, event) and hands it over on the Message, so the wire shape and
	// the resolved object are the same type by construction.
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

type discordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"`
	URL         string         `json:"url,omitempty"`
	Author      *discordAuthor `json:"author,omitempty"`
	Fields      []discordField `json:"fields,omitempty"`
	Thumbnail   *discordImage  `json:"thumbnail,omitempty"`
	Image       *discordImage  `json:"image,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

// discordField is a type alias for Field so the Discord JSON encoder can
// share the same struct without keeping a parallel duplicate definition.
// Field carries the JSON tags directly. Audit reports/small-packages.md.
type discordField = Field

type discordAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// footerText renders the embed footer. "Moombox Go" was a rewrite-era suffix
// that identified nothing; with several channels and several jobs landing in
// one Discord channel, the platform and the job id are what let an operator
// tell two embeds apart without opening the link.
func footerText(opts SendOptions) string {
	text := "Moombox"
	if opts.Platform != "" {
		text += " · " + opts.Platform
	}
	if opts.JobID != "" {
		text += " · " + opts.JobID
	}
	return text
}

// MentionParse maps a configured mention to the allowed_mentions object that
// makes it actually ping, or nil for a form we do not recognise — which keeps
// an unvalidated config string from becoming an unrestricted ping. buildTargets
// calls it once per target, and Manager.Send puts what mentionFor returns onto
// the Message.
func MentionParse(mention string) *AllowedMentions {
	switch {
	case mention == "@everyone" || mention == "@here":
		return &AllowedMentions{Parse: []string{"everyone"}}
	case strings.HasPrefix(mention, "<@&") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@&"), ">")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Roles: []string{id}}
	case strings.HasPrefix(mention, "<@") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@"), ">")
		// <@!id> is the legacy nickname form; Discord still accepts it in
		// content and the id is what allowed_mentions needs either way.
		id = strings.TrimPrefix(id, "!")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Users: []string{id}}
	}
	return nil
}

// toDiscordEmbed renders ONE Embed as the wire struct, clamped.
//
// Package-visible rather than inlined into buildPayload so the batcher can
// size an embed with the SAME clamp the payload will apply: one definition of
// "how big is this embed", and no drift between what a batch measured and what
// it later sends.
func toDiscordEmbed(e Embed) discordEmbed {
	opts := e.Opts
	embed := discordEmbed{
		Title:       e.Title,
		Description: e.Description,
		Color:       e.Color,
		Footer:      &discordFooter{Text: footerText(opts)},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	if opts.URL != "" {
		embed.URL = opts.URL
	}

	// Discord rejects an author object with no name (a permanent 400), so a
	// half-filled Author is dropped rather than sent.
	if opts.Author != nil && opts.Author.Name != "" {
		embed.Author = &discordAuthor{
			Name:    opts.Author.Name,
			URL:     opts.Author.URL,
			IconURL: opts.Author.IconURL,
		}
	}

	if opts.Thumbnail != "" {
		embed.Thumbnail = &discordImage{URL: opts.Thumbnail}
	}

	if opts.Image != "" {
		embed.Image = &discordImage{URL: opts.Image}
	}

	if len(e.Fields) > 0 {
		// discordField is a type alias for Field, so this is a direct copy
		// rather than an element-by-element conversion. It stays a copy
		// because the clamp below rewrites values in place; the caller's own
		// buffer is already protected a layer up, where Manager.Send copies
		// into the queued item.
		embed.Fields = append([]discordField(nil), e.Fields...)
	}

	// One clamp for all ~36 send sites. Over ANY Discord limit is a 400, and
	// the retry ladder treats a non-429 4xx as permanent, so an unclamped
	// embed is a silently dropped alert.
	//
	// PER EMBED. Discord's 6000-character total is per MESSAGE, which for the
	// single-embed message every producer sends is the same budget; keeping a
	// MULTI-embed message inside it is the batcher's job, because only the
	// batcher can decide which embed not to add.
	clampEmbed(&embed)
	return embed
}

// buildPayload assembles the message JSON shared by Send and SendOnce.
func buildPayload(msg Message) ([]byte, error) {
	payload := discordPayload{Embeds: make([]discordEmbed, 0, len(msg.Embeds))}
	for _, e := range msg.Embeds {
		payload.Embeds = append(payload.Embeds, toDiscordEmbed(e))
	}

	// A mention rides the message content, never the embed — which is also why
	// it is read from the MESSAGE and not from an embed's opts: a batch of ten
	// pings once. A nil MentionAllowed is the per-event gate N2b fills from
	// mention_events: no object, no ping. MentionParse is where an unrecognised
	// form becomes that nil.
	if msg.MentionAllowed != nil && msg.Mention != "" {
		payload.Content = msg.Mention
		payload.AllowedMentions = msg.MentionAllowed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal discord payload: %w", err)
	}
	return body, nil
}

// SendOnce performs a single delivery attempt with NO retries.
//
// Two callers: SendTest, where an interactive settings flow wants the
// immediate outcome (surfacing a 429 beats sleeping through its Retry-After),
// and the per-target queue during shutdown, where the 10s force-exit leaves no
// room for the ladder.
//
// A known-empty rate bucket is therefore honoured only when it refills within
// shutdownBucketWaitCap (2s); a window further away than that is not waited
// for at all — this posts, and the 429 Discord answers with drops the item
// just as the wait would have, without spending the force-exit's remaining
// seconds or starving the items queued behind this one. So the bound stays
// shutdownBucketWaitCap plus the single-attempt request timeout (~15s).
func (d *DiscordWebhook) SendOnce(msg Message) error {
	body, err := buildPayload(msg)
	if err != nil {
		return err
	}
	d.waitForBucketWithin(shutdownBucketWaitCap)
	r, err := d.do(http.MethodPost, d.URL, body, false)
	d.noteBucket(r)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case r.status < 400:
		return nil
	case r.status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
	default:
		return discordStatusErr(r.status, r.snippet)
	}
}

// Send sends a Discord webhook message.
func (d *DiscordWebhook) Send(msg Message) error {
	body, err := buildPayload(msg)
	if err != nil {
		return err
	}
	_, err = d.deliver(http.MethodPost, d.URL, body, false)
	return err
}

// deliver runs the bounded delivery loop against one method+URL and returns
// the successful attempt's response body (nil unless wantBody).
//
// Transport errors and Discord 5xx retry on the fixed backoff schedule, 429
// honors a validated Retry-After, other 4xx are permanent. POST and PATCH go
// through this ONE loop on purpose: per the Discord API docs the bucket's
// top-level resource is webhook_id + webhook_token, so an edit spends the same
// budget as a post and must obey the same schedule.
//
// Alerts exist precisely for flaky moments; the previous single-shot behavior
// dropped e.g. a "recording failed" embed on the first connection reset.
func (d *DiscordWebhook) deliver(method, endpoint string, body []byte, wantBody bool) ([]byte, error) {
	var lastErr error
	var slept time.Duration
	for attempt := 1; ; attempt++ {
		// Pre-emptive: if the last response said the bucket was empty, wait
		// out its window instead of spending one of three attempts on the 429
		// Discord has already promised.
		d.waitForBucket()
		r, err := d.do(method, endpoint, body, wantBody)
		d.noteBucket(r)

		var delay time.Duration
		switch {
		case err != nil:
			lastErr = fmt.Errorf("discord webhook request: %w", err)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		case r.status < 400:
			return r.body, nil
		case r.status == http.StatusTooManyRequests:
			// Validate in FLOAT space before converting: values past ~9.2e9s
			// (or Inf) overflow time.Duration to a NEGATIVE on amd64, which
			// would slip past a Duration-space cap check and turn the sleep
			// into a zero-delay hammer. !(secs > 0) is deliberately NaN-proof.
			secs, parseErr := strconv.ParseFloat(r.retryAfter, 64)
			if parseErr != nil || !(secs > 0) || secs > discordRetryAfterCap.Seconds() {
				// Missing, malformed, or absurd Retry-After — surface the
				// 429 directly rather than guessing a sleep.
				return nil, fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			}
			lastErr = fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			delay = time.Duration(secs * float64(time.Second))
		case r.status >= 500:
			lastErr = discordStatusErr(r.status, r.snippet)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return nil, discordStatusErr(r.status, r.snippet)
		}

		if attempt == discordMaxAttempts {
			return nil, fmt.Errorf("%w (gave up after %d attempts)", lastErr, discordMaxAttempts)
		}
		if slept+delay > discordMaxSleepTotal {
			// Cumulative-sleep budget exhausted (e.g. a second large
			// Retry-After) — bound this target queue's hold instead of
			// waiting out an extended rate-limit.
			return nil, fmt.Errorf("%w (retry budget exhausted after %d attempts)", lastErr, attempt)
		}
		slept += delay
		time.Sleep(delay)
	}
}

// discordErrSnippet renders a response-body prefix as a single-line, printable
// error fragment. The body is REMOTE input that lands in log lines and in
// SendTest's HTTP response, so newlines (which would forge a log line), other
// control characters, and the replacement rune a truncated multi-byte tail
// decodes to all collapse to spaces; runs of whitespace collapse to one.
func discordErrSnippet(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteRune(r)
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// discordStatusErr renders a non-2xx webhook response, quoting the body prefix
// when Discord sent one. Used by every site that reports a status — 4xx and
// 5xx alike: the 5xx text is what an operator sees after the retry budget is
// spent, which is precisely when the reason matters.
//
// The concrete *discordHTTPError (discord_edit.go) keeps the status and
// snippet reachable with errors.As after the retry loop has wrapped the error,
// which is how the edit path tells a 404/10008 from every other refusal. The
// rendered text is unchanged.
func discordStatusErr(status int, snippet string) error {
	return &discordHTTPError{Status: status, Snippet: snippet}
}

// waitForBucket honours what Discord last told us about this webhook's rate
// bucket: an X-RateLimit-Remaining of 0 means the NEXT request 429s until the
// window resets, so sleeping through it costs one wait and saves an attempt
// out of the three this notification has. Per Discord API docs
// (topics/rate-limits.mdx) the bucket is discoverable only from these headers —
// there is no published numeric cap.
//
// Capped by discordRetryAfterCap for the same reason a 429's Retry-After is:
// an absurd reset must not park a target's whole queue into next week. The
// retry ladder is what makes the wait worth taking — a single-attempt send has
// no second chance to spend it on, which is why SendOnce calls
// waitForBucketWithin instead.
func (d *DiscordWebhook) waitForBucket() {
	wait := d.bucketWait()
	if wait <= 0 {
		return
	}
	if wait > discordRetryAfterCap {
		wait = discordRetryAfterCap
	}
	time.Sleep(wait)
}

// waitForBucketWithin honours the empty-bucket window only when it closes
// within max, and otherwise returns at once rather than clamping to max.
// Waiting part of a window buys nothing: the request still lands inside it and
// still 429s, so a caller that cannot afford the whole wait is better off
// spending nothing. SendOnce is that caller.
func (d *DiscordWebhook) waitForBucketWithin(max time.Duration) {
	wait := d.bucketWait()
	if wait <= 0 || wait > max {
		return
	}
	time.Sleep(wait)
}

// bucketWait reports how long until this webhook's bucket refills, or 0 when
// it was never marked empty (or the window has already passed).
func (d *DiscordWebhook) bucketWait() time.Duration {
	d.bucketMu.Lock()
	until := d.bucketRefillsAt
	d.bucketMu.Unlock()
	if until.IsZero() {
		return 0
	}
	return time.Until(until)
}

// noteBucket records (or clears) the empty-bucket deadline from one response.
//
// A 429 is deliberately EXCLUDED: Discord sets Remaining: 0 on one, and the
// ladder already honours its Retry-After, so arming the pre-emptive sleep from
// the same response would wait the window twice and spend the cumulative-sleep
// budget on the duplicate.
func (d *DiscordWebhook) noteBucket(r discordResponse) {
	if r.status == http.StatusTooManyRequests || r.rateRemain == "" {
		return
	}
	remaining, err := strconv.Atoi(r.rateRemain)
	if err != nil {
		return
	}
	if remaining > 0 {
		d.bucketMu.Lock()
		d.bucketRefillsAt = time.Time{}
		d.bucketMu.Unlock()
		return
	}
	// Validate in FLOAT space before converting, for the reason the 429 arm
	// documents: a value past ~9.2e9s (or Inf) overflows time.Duration to a
	// NEGATIVE on amd64. !(secs > 0) is deliberately NaN-proof.
	secs, err := strconv.ParseFloat(r.rateReset, 64)
	if err != nil || !(secs > 0) {
		return
	}
	if secs > discordRetryAfterCap.Seconds() {
		secs = discordRetryAfterCap.Seconds()
	}
	// bucketSkewPad covers the header's millisecond rounding — see its comment.
	d.bucketMu.Lock()
	d.bucketRefillsAt = time.Now().Add(time.Duration(secs*float64(time.Second)) + bucketSkewPad)
	d.bucketMu.Unlock()
}

// discordResponse is what one attempt learned. Grouped rather than returned as
// five bare values: the rate-limit pair has to travel with the status, and a
// six-value signature is where a caller starts transposing arguments.
type discordResponse struct {
	status     int    // 0 on a transport error
	retryAfter string // Retry-After, 429 only
	rateRemain string // X-RateLimit-Remaining
	rateReset  string // X-RateLimit-Reset-After, in seconds
	snippet    string // sanitised body prefix, >= 400 only
	body       []byte // the 2xx response body, read only when wantBody is set
}

// do performs one webhook request attempt against method+endpoint. Same
// returns post had, plus the 2xx response body when wantBody is set.
//
// The method and target are parameters rather than the fixed POST d.URL
// because the edit path issues PATCH against the per-message route; per the
// Discord API docs both share the webhook's rate-limit bucket, so they must
// share this one request path and the loop above it.
//
// The parameter is named endpoint, not url: the body reaches for *url.Error
// to redact the token out of a transport error, and a parameter named url
// would shadow the net/url import.
func (d *DiscordWebhook) do(method, endpoint string, body []byte, wantBody bool) (discordResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return discordResponse{}, fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body (the queue's failure
		// log, SendTest's route response, retry-loop wrap all flow through
		// here).
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return discordResponse{}, fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return discordResponse{}, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	out := discordResponse{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		rateRemain: resp.Header.Get("X-RateLimit-Remaining"),
		rateReset:  resp.Header.Get("X-RateLimit-Reset-After"),
	}
	if resp.StatusCode >= 400 {
		// Read the reason BEFORE the deferred drain throws the rest away.
		// The read error is intentionally ignored: a partial read (e.g. the
		// connection drops mid-body) still yields whatever prefix arrived,
		// which is a usable snippet — better than discarding it outright.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		out.snippet = discordErrSnippet(b)
	} else if wantBody {
		// Only the ?wait=true POST needs this; every other call leaves the
		// body to the deferred drain.
		out.body, _ = io.ReadAll(io.LimitReader(resp.Body, discordMessageBodyBytes))
	}
	return out, nil
}

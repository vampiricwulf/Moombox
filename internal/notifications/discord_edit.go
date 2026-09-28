package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrUnknownMessage is what patchMessage answers when Discord refuses an edit
// with 404 / code 10008 ("Unknown Message"): the message this webhook created
// is gone — deleted in the channel, or the channel purged. It is the ONE 4xx
// on the edit path that is not permanent; the caller posts a fresh message and
// overwrites the stored id.
//
// A 404 naming "Unknown Webhook" (code 10015) is deliberately NOT this: the
// webhook itself was revoked, and re-posting would fail the same way forever.
var ErrUnknownMessage = errors.New("discord: unknown message")

// waitQuery is the last-resort suffix execWaitURL falls back to when d.URL
// will not parse. Per the Discord API docs (resources/webhook.mdx),
// ?wait=true "waits for server confirmation of message send before response,
// and returns the created message body" — the only way to learn the id an edit
// needs.
const waitQuery = "?wait=true"

// unknownMessageCode matches Discord's numeric error code for "Unknown
// Message" as a JSON FIELD, not as bare digits: a refusal body that echoes the
// requested path can carry 10008 inside a message snowflake
// ("/messages/1418889000000010008"), and a substring test on the digits reads
// that as recoverable — re-POSTing forever to a revoked webhook, which is the
// exact outcome patchMessage's 10015 row exists to prevent. \s* absorbs
// discordErrSnippet's whitespace collapsing, and \b keeps 100081 from matching.
var unknownMessageCode = regexp.MustCompile(`"code"\s*:\s*10008\b`)

// discordHTTPError carries the status and sanitised body prefix of a Discord
// refusal so a caller can branch on WHICH refusal it got. Only the edit path
// needs that today (404/10008 apart from every other 4xx), but the type is
// what discordStatusErr returns everywhere, so the information is never lost
// on the way up.
type discordHTTPError struct {
	Status  int
	Snippet string
}

func (e *discordHTTPError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("discord webhook returned %d", e.Status)
	}
	return fmt.Sprintf("discord webhook returned %d: %s", e.Status, e.Snippet)
}

// isUnknownMessage reports whether err is Discord's "this message no longer
// exists" refusal. The snippet is required: a bare 404 with no body could be a
// revoked webhook or a proxy, and re-posting on it would be a guess.
func isUnknownMessage(err error) bool {
	var he *discordHTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.Status != http.StatusNotFound {
		return false
	}
	return strings.Contains(he.Snippet, "Unknown Message") || unknownMessageCode.MatchString(he.Snippet)
}

// execWaitURL is the webhook's execute route with wait=true added to whatever
// query the configured URL already carries.
//
// Built through net/url rather than by appending a literal "?wait=true":
// discordWebhookRe (manager.go) is unanchored, so ValidateURL accepts a
// webhook URL with a query — and ?thread_id= is Discord's documented way to
// post into a forum thread. Concatenating would fold wait into THAT
// parameter's value ("thread_id=456?wait=true"), Discord would answer its
// non-wait 204, and edit mode would silently post a duplicate per event that
// can never be edited. A fragment is dropped for the same reason: it never
// reaches the server, so it would swallow the suffix entirely.
func (d *DiscordWebhook) execWaitURL() string {
	u, err := url.Parse(d.URL)
	if err != nil {
		return d.URL + waitQuery
	}
	q := u.Query()
	q.Set("wait", "true")
	u.RawQuery, u.Fragment = q.Encode(), ""
	return u.String()
}

// messageURL is the per-message edit endpoint,
// PATCH /webhooks/{id}/{token}/messages/{message_id} (Discord API docs,
// resources/webhook.mdx). The route is a PATH suffix, so it is appended to the
// parsed URL's path rather than to the whole string: a configured
// "…/TOKEN?thread_id=456" would otherwise put "/messages/999" inside the query
// and PATCH the execute route. thread_id survives (it identifies the forum
// thread on both routes); wait never does — the edit route takes no such
// parameter. Going through u.String() also escapes messageID, which reaches
// here straight out of the database column with no shape validation.
func (d *DiscordWebhook) messageURL(messageID string) string {
	u, err := url.Parse(d.URL)
	if err != nil {
		return strings.TrimRight(d.URL, "/") + "/messages/" + url.PathEscape(messageID)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/messages/" + messageID
	u.RawPath, u.Fragment = "", ""
	q := u.Query()
	q.Del("wait")
	u.RawQuery = q.Encode()
	return u.String()
}

// postWait creates a message and returns its id, for a target that will later
// rewrite it in place. Same loop, same retry schedule and same rate-limit
// bucket as an ordinary Send — the only differences are wait=true and that the
// response body is read.
func (d *DiscordWebhook) postWait(body []byte) (string, error) {
	respBody, err := d.deliver(http.MethodPost, d.execWaitURL(), body, true)
	if err != nil {
		return "", err
	}
	return parseCreatedID(respBody)
}

// patchMessage rewrites a message this webhook created. A 404/10008 comes back
// as ErrUnknownMessage so the caller can re-post; every other refusal is
// permanent and reaches the caller unchanged.
func (d *DiscordWebhook) patchMessage(messageID string, body []byte) error {
	_, err := d.deliver(http.MethodPatch, d.messageURL(messageID), body, false)
	return asEditRefusal(messageID, err)
}

// postWaitOnce and patchMessageOnce are the shutdown twins of the two above:
// ONE attempt, no backoff, no Retry-After sleep. The owner's ruling caps a
// graceful shutdown at 10 s, and a single edit-mode job running the full
// three-attempt loop could spend all of it — the same reason SendOnce exists
// beside Send. The queue selects these while it is shutting down.
func (d *DiscordWebhook) postWaitOnce(body []byte) (string, error) {
	// Mirrors SendOnce exactly: honour a known-empty bucket only if it
	// refills inside the shutdown cap, learn from the response, and report a
	// 429 as the rate limit it is rather than as an anonymous status.
	d.waitForBucketWithin(shutdownBucketWaitCap)
	r, err := d.do(http.MethodPost, d.execWaitURL(), body, true)
	d.noteBucket(r)
	switch {
	case err != nil:
		return "", fmt.Errorf("discord webhook request: %w", err)
	case r.status == http.StatusTooManyRequests:
		return "", fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
	case r.status >= 400:
		return "", discordStatusErr(r.status, r.snippet)
	}
	return parseCreatedID(r.body)
}

func (d *DiscordWebhook) patchMessageOnce(messageID string, body []byte) error {
	d.waitForBucketWithin(shutdownBucketWaitCap)
	r, err := d.do(http.MethodPatch, d.messageURL(messageID), body, false)
	d.noteBucket(r)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case r.status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
	case r.status >= 400:
		return asEditRefusal(messageID, discordStatusErr(r.status, r.snippet))
	}
	return nil
}

// asEditRefusal maps Discord's "this message no longer exists" onto
// ErrUnknownMessage and passes every other refusal through unchanged. One
// helper so the retrying and single-attempt edit paths cannot disagree about
// which 404 is recoverable.
//
// TWO %w verbs: the caller matches ErrUnknownMessage with errors.Is, and the
// status and snippet stay reachable with errors.As on *discordHTTPError — the
// wrap must not be the one path in the package where that information is lost.
func asEditRefusal(messageID string, err error) error {
	if err == nil {
		return nil
	}
	if isUnknownMessage(err) {
		return fmt.Errorf("%w (message %s): %w", ErrUnknownMessage, messageID, err)
	}
	return err
}

// parseCreatedID pulls the id out of a wait=true response.
func parseCreatedID(respBody []byte) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("discord: parse created message: %w", err)
	}
	if created.ID == "" {
		// Storing "" would make every later edit target /messages/ — fail the
		// send instead and let the next lifecycle event post again.
		return "", errors.New("discord: created message carried no id")
	}
	return created.ID, nil
}

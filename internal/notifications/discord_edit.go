package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// waitQuery makes a webhook POST answer with the created message instead of a
// bare 204. Per the Discord API docs (resources/webhook.mdx), ?wait=true
// "waits for server confirmation of message send before response, and returns
// the created message body" — the only way to learn the id an edit needs.
const waitQuery = "?wait=true"

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
	return strings.Contains(he.Snippet, "Unknown Message") || strings.Contains(he.Snippet, "10008")
}

// messageURL is the per-message edit endpoint,
// PATCH /webhooks/{id}/{token}/messages/{message_id} (Discord API docs,
// resources/webhook.mdx). d.URL is already the resolved
// https://discord.com/api/webhooks/ID/TOKEN form, so the route is a suffix.
func (d *DiscordWebhook) messageURL(messageID string) string {
	return strings.TrimRight(d.URL, "/") + "/messages/" + messageID
}

// postWait creates a message and returns its id, for a target that will later
// rewrite it in place. Same loop, same retry schedule and same rate-limit
// bucket as an ordinary Send — the only differences are ?wait=true and that
// the response body is read.
func (d *DiscordWebhook) postWait(body []byte) (string, error) {
	respBody, err := d.deliver(http.MethodPost, d.URL+waitQuery, body, true)
	if err != nil {
		return "", err
	}
	return parseCreatedID(respBody)
}

// patchMessage rewrites a message this webhook created. A 404/10008 comes back
// as ErrUnknownMessage so the caller can re-post; every other refusal is
// permanent and reaches the caller unchanged.
func (d *DiscordWebhook) patchMessage(messageID string, body []byte) error {
	return asEditRefusal(messageID, mustErr(d.deliver(http.MethodPatch, d.messageURL(messageID), body, false)))
}

// postWaitOnce and patchMessageOnce are the shutdown twins of the two above:
// ONE attempt, no backoff, no Retry-After sleep. The owner's ruling caps a
// graceful shutdown at 10 s, and a single edit-mode job running the full
// three-attempt loop could spend all of it — the same reason SendOnce exists
// beside Send. The queue selects these while it is shutting down.
func (d *DiscordWebhook) postWaitOnce(body []byte) (string, error) {
	// Mirrors SendOnce exactly: honour a known-empty bucket only if it
	// refills inside the shutdown cap, then learn from the response.
	d.waitForBucketWithin(shutdownBucketWaitCap)
	r, err := d.do(http.MethodPost, d.URL+waitQuery, body, true)
	d.noteBucket(r)
	switch {
	case err != nil:
		return "", fmt.Errorf("discord webhook request: %w", err)
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
	case r.status >= 400:
		return asEditRefusal(messageID, discordStatusErr(r.status, r.snippet))
	}
	return nil
}

// asEditRefusal maps Discord's "this message no longer exists" onto
// ErrUnknownMessage and passes every other refusal through unchanged. One
// helper so the retrying and single-attempt edit paths cannot disagree about
// which 404 is recoverable.
func asEditRefusal(messageID string, err error) error {
	if err == nil {
		return nil
	}
	if isUnknownMessage(err) {
		return fmt.Errorf("%w (message %s): %v", ErrUnknownMessage, messageID, err)
	}
	return err
}

// mustErr drops deliver's unused body return at the PATCH call sites.
func mustErr(_ []byte, err error) error { return err }

// parseCreatedID pulls the id out of a ?wait=true response.
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

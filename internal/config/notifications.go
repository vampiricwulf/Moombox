package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MentionForm classifies a validated per-target mention token.
type MentionForm int

const (
	MentionNone MentionForm = iota
	MentionRole
	MentionUser
	MentionEveryone
	MentionHere
)

// mentionRoleRe matches a role mention: <@&ROLE_ID>.
// mentionUserRe matches a user mention, including the legacy "nickname"
// spelling <@!USER_ID> that Discord clients have emitted for years — a
// working paste is not something a validator should reject.
var (
	mentionRoleRe = regexp.MustCompile(`^<@&(\d{1,20})>$`)
	mentionUserRe = regexp.MustCompile(`^<@(!?)(\d{1,20})>$`)
)

// defaultMentionEvents is the owner's 2026-09-27 Q5 ruling for which events a
// mention rides along with when a target sets Mention but never writes
// mention_events. Package-private: callers get a copy via
// DefaultMentionEvents so one target's edit can't corrupt every other
// target's defaults.
var defaultMentionEvents = []string{
	"error", "auth", "disk_critical", "update_failed", "crash_recovered", "sidecar_down",
}

// DefaultMentionEvents returns a fresh copy of the owner's default mention
// filter (2026-09-27 ruling Q5).
func DefaultMentionEvents() []string {
	out := make([]string, len(defaultMentionEvents))
	copy(out, defaultMentionEvents)
	return out
}

// IsEnabled reports whether this notification target delivers. Absent means
// enabled, exactly as ChannelConfig.IsEnabled treats an absent channel flag.
func (n *NotificationConfig) IsEnabled() bool {
	if n.Enabled == nil {
		return true
	}
	return *n.Enabled
}

// ResolveMentionEvents returns the effective mention filter for this target:
// nil when there is no mention to ping with, the default six when Mention is
// set but MentionEvents was never written, the stored (possibly empty) list
// otherwise. An explicit empty list means "never" and must not fall back to
// the defaults.
func (n *NotificationConfig) ResolveMentionEvents() []string {
	if n.Mention == "" {
		return nil
	}
	if n.MentionEvents == nil {
		return DefaultMentionEvents()
	}
	return *n.MentionEvents
}

// ParseMention validates a per-target mention token and reports its form.
// Accepted forms: "<@&ROLE_ID>" (role), "<@USER_ID>" or the legacy
// "<@!USER_ID>" nickname spelling (user, canonicalised to "<@USER_ID>"),
// "@everyone" and "@here". An empty string is the documented "no mention"
// case, not an error.
func ParseMention(raw string) (canonical string, form MentionForm, id string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", MentionNone, "", nil
	}
	if s == "@everyone" {
		return s, MentionEveryone, "", nil
	}
	if s == "@here" {
		return s, MentionHere, "", nil
	}
	if m := mentionRoleRe.FindStringSubmatch(s); m != nil {
		return "<@&" + m[1] + ">", MentionRole, m[1], nil
	}
	if m := mentionUserRe.FindStringSubmatch(s); m != nil {
		return "<@" + m[2] + ">", MentionUser, m[2], nil
	}
	return "", MentionNone, "", fmt.Errorf(
		"mention %q is not a role (<@&ID>), a user (<@ID>), @everyone, or @here", raw)
}

// ValidatePublicURL canonicalises a network.public_url value. Empty (or
// blank) input is the documented "unset" case and returns ("", nil). A
// non-empty value must be an absolute http(s) URL with a host and no query,
// fragment, or userinfo — the notification manager appends "/#job=<id>" to
// this value in every job embed's title link, so a trailing slash is trimmed
// and anything that would collide with or leak through that link is
// rejected. A value with an '@' anywhere in it is refused as userinfo before
// it is parsed, so a path that happens to hold one goes too.
func ValidatePublicURL(raw string) (canonical string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	// This error is shown and logged — the boot line for a value Load
	// replaced, the settings API's field error, the TUI form — so it must
	// never carry a password, and url.Parse cannot be trusted with one.
	// net/url ends the authority at the first '/', '?' or '#', so a
	// password holding any of them ("https://u:pa/ss@host") is read as
	// host:port and refused with `invalid port ":pa" after host`, quoting
	// it; with '#' it can even parse ("https://u:1234#pw@host" is host u,
	// port 1234 and a fragment). Every userinfo needs an '@', so an '@'
	// anywhere is refused first, with a message that quotes nothing — a
	// public_url has no use for one: not in the authority, and the query
	// and fragment are refused below anyway.
	if strings.Contains(s, "@") {
		return "", fmt.Errorf("must not contain userinfo (an '@')")
	}
	u, err := url.Parse(s)
	if err != nil {
		// url.Parse's error quotes the whole value. Its cause alone says
		// what is wrong, and with no '@' left in the value the host or
		// port slot it may quote is no one's password.
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			err = uerr.Err
		}
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("must not contain a query string")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("must not contain a fragment")
	}
	u.Scheme = scheme
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

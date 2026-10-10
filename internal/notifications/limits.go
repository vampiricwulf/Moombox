package notifications

import (
	"strings"
	"unicode/utf8"
)

// Discord's embed limits, per the Discord API docs (resources/message.mdx).
// Exceeding ANY of them is a 400, and discord.go treats a non-429 4xx as
// permanent — so before these existed, one over-long error string dropped the
// whole alert after a single attempt with nothing in the log but the status.
const (
	limitTitle       = 256
	limitDescription = 4096
	limitFieldName   = 256
	limitFieldValue  = 1024
	limitFooter      = 2048
	limitAuthorName  = 256
	limitFields      = 25
	// limitTotal is the sum across title, description, every field name and
	// value, the footer text and the author name, in characters.
	limitTotal = 6000
)

// clampMarker is appended to anything ClampRunes shortens. One rune, so it
// costs one of the budget it is protecting.
const clampMarker = "…"

// ClampRunes shortens s to at most limit CHARACTERS, cutting on a rune
// boundary and marking the cut with "…".
//
// Exported because producers need the same cut for their own excerpts — the
// job Description excerpt in internal/worker/orchestrator_mux.go was cut by
// BYTE (desc[:297]), which splits a Japanese description mid-rune and makes
// encoding/json emit U+FFFD. Discord counts characters, not bytes, so a
// byte-based cut is also the wrong unit for the limit it was aiming at.
func ClampRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	kept := 0
	for i := range s {
		if kept == limit-1 {
			return s[:i] + clampMarker
		}
		kept++
	}
	// Unreachable for a string longer than limit runes, but a loop that can
	// fall through must still answer.
	return s + clampMarker
}

// markdownEscaper escapes the characters Discord's markdown subset gives
// meaning to inside an embed description or field value (per the Discord API
// docs, reference.mdx). The BACKSLASH IS FIRST and that ordering is
// load-bearing: strings.NewReplacer matches at each position in one pass, so
// the replacements never re-scan each other's output — but a hand-rolled
// sequential ReplaceAll that escaped the backslash last would double every
// escape it had just written.
//
// `<` and `>` are here because Discord's <t:…>, <@…>, <#…> and <:emoji:id>
// forms all open with one; a stream title containing "<3" would otherwise
// start something that swallows the rest of the line.
//
// `[` and `]` are here because Discord renders a masked link, [text](url), in
// descriptions and field values: a stream title "[Claim prize](https://…)"
// became a clickable link whose target the reader never sees.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"*", `\*`,
	"_", `\_`,
	"~", `\~`,
	"|", `\|`,
	"`", "\\`",
	"<", `\<`,
	">", `\>`,
	"[", `\[`,
	"]", `\]`,
)

// EscapeMarkdown neutralises Discord markdown in job-supplied text.
//
// APPLY IT TO job-supplied strings — stream titles, error text, file paths,
// channel names. Do NOT apply it to static titles Moombox writes itself, and
// never to a field that carries <t:…> markup ON PURPOSE (the Scheduled For,
// Starts At, Old/New Time and Outage Alert fields): escaping those turns a
// live relative timestamp into literal text.
//
// An embed can never mention anyone (see buildPayload's mention handling), so
// what an unescaped title can do is limited to how it renders: italics, a
// spoiler, a heading — or a masked link that hides where it points, which is
// why the brackets are escaped too.
func EscapeMarkdown(s string) string {
	if s == "" {
		return ""
	}
	escaped := markdownEscaper.Replace(s)
	// #, - and > are special only at the START of a line (heading, list item,
	// block quote). > is already escaped everywhere by the replacer above;
	// these two are not, because escaping every hyphen in a title would be
	// noise an operator reads.
	lines := strings.Split(escaped, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "-") {
			lines[i] = `\` + ln
		}
	}
	return strings.Join(lines, "\n")
}

// embedRunes is the character count Discord applies its 6000 total against:
// title, description, every field name and value, the footer text and the
// author name. URLs, the colour and the timestamp are not counted.
func embedRunes(e *discordEmbed) int {
	total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	if e.Footer != nil {
		total += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		total += utf8.RuneCountInString(e.Author.Name)
	}
	return total
}

// clampEmbed brings one embed inside every Discord limit, in place.
//
// The ORDER of the total-budget pass is the decision: trailing fields go
// first, then the description is trimmed, and the title is never touched. A
// reader identifies an embed by its title in a notification popup before they
// open Discord at all, so the title is the last thing worth spending; fields
// are the cheapest because the ones that overflow are the trailing optional
// enrichments, not the Channel/ID pair at the front.
func clampEmbed(e *discordEmbed) {
	e.Title = ClampRunes(e.Title, limitTitle)
	e.Description = ClampRunes(e.Description, limitDescription)
	if e.Author != nil {
		e.Author.Name = ClampRunes(e.Author.Name, limitAuthorName)
	}
	if e.Footer != nil {
		e.Footer.Text = ClampRunes(e.Footer.Text, limitFooter)
	}
	// Discord answers a field with an empty name or value with a 400, which
	// deliver treats as permanent: the whole embed is dropped. Several sends
	// fill a field straight from the row (a channel name, a video ID, a
	// title) that a manually-added or channel-less job leaves empty, so the
	// field goes rather than the message.
	kept := make([]discordField, 0, len(e.Fields))
	for _, f := range e.Fields {
		if strings.TrimSpace(f.Name) != "" && strings.TrimSpace(f.Value) != "" {
			kept = append(kept, f)
		}
	}
	e.Fields = kept
	if len(e.Fields) > limitFields {
		e.Fields = e.Fields[:limitFields]
	}
	for i := range e.Fields {
		e.Fields[i].Name = ClampRunes(e.Fields[i].Name, limitFieldName)
		e.Fields[i].Value = ClampRunes(e.Fields[i].Value, limitFieldValue)
	}

	for embedRunes(e) > limitTotal && len(e.Fields) > 0 {
		e.Fields = e.Fields[:len(e.Fields)-1]
	}
	if over := embedRunes(e) - limitTotal; over > 0 {
		e.Description = ClampRunes(e.Description, utf8.RuneCountInString(e.Description)-over)
	}
}

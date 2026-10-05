package notifications

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// jp repeats a 3-byte Japanese rune n times. Japanese metadata is the norm for
// this project (cmd/moombox/job_progress_test.go), and a byte-indexed cut
// splits one of these mid-rune — encoding/json then emits U+FFFD and the
// operator reads a mojibake title.
func jp(n int) string { return strings.Repeat("あ", n) }

// TestClampRunesCutsOnRuneBoundaries is the helper both the payload clamp and
// the worker's Description excerpt go through.
//
// THE MUTANT: `s[:limit-1]` instead of a rune walk. Every multi-byte subtest
// then produces invalid UTF-8.
func TestClampRunesCutsOnRuneBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"under the limit is untouched", "abc", 10, "abc"},
		{"exactly at the limit is untouched", "abcde", 5, "abcde"},
		{"ascii over the limit", "abcdef", 5, "abcd…"},
		{"japanese over the limit", jp(6), 5, jp(4) + "…"},
		{"japanese exactly at the limit", jp(5), 5, jp(5)},
		{"limit 1 is just the marker", "abcdef", 1, "…"},
		{"limit 0 is empty", "abcdef", 0, ""},
		{"negative limit is empty", "abcdef", -3, ""},
		{"empty input", "", 10, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampRunes(tc.in, tc.limit)
			if got != tc.want {
				t.Errorf("ClampRunes(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ClampRunes(%q, %d) produced invalid UTF-8: %q", tc.in, tc.limit, got)
			}
			if n := utf8.RuneCountInString(got); tc.limit >= 0 && n > tc.limit {
				t.Errorf("ClampRunes(%q, %d) returned %d runes", tc.in, tc.limit, n)
			}
		})
	}
}

// TestClampEmbedHonoursEveryDiscordLimit walks the per-field caps from the
// Discord API docs (resources/message.mdx): title 256, description 4096, field
// name 256, field value 1024, footer 2048, author name 256, 25 fields.
// Every one of them is a permanent 400 when exceeded, and nothing clamped.
func TestClampEmbedHonoursEveryDiscordLimit(t *testing.T) {
	e := &discordEmbed{
		Title:       jp(400),
		Description: jp(5000),
		Author:      &discordAuthor{Name: jp(400)},
		Footer:      &discordFooter{Text: jp(3000)},
	}
	for i := range 40 {
		e.Fields = append(e.Fields, discordField{Name: jp(400), Value: jp(2000)})
		_ = i
	}
	clampEmbed(e)

	if n := utf8.RuneCountInString(e.Title); n != limitTitle {
		t.Errorf("title = %d runes, want %d", n, limitTitle)
	}
	if n := utf8.RuneCountInString(e.Author.Name); n != limitAuthorName {
		t.Errorf("author name = %d runes, want %d", n, limitAuthorName)
	}
	if n := utf8.RuneCountInString(e.Footer.Text); n != limitFooter {
		t.Errorf("footer = %d runes, want %d", n, limitFooter)
	}
	if len(e.Fields) > limitFields {
		t.Errorf("fields = %d, want <= %d", len(e.Fields), limitFields)
	}
	for i, f := range e.Fields {
		if n := utf8.RuneCountInString(f.Name); n > limitFieldName {
			t.Errorf("field %d name = %d runes", i, n)
		}
		if n := utf8.RuneCountInString(f.Value); n > limitFieldValue {
			t.Errorf("field %d value = %d runes", i, n)
		}
	}
	if !utf8.ValidString(e.Description) {
		t.Error("description is not valid UTF-8 after the clamp")
	}
}

// TestClampEmbedTrimsToTheTotalBudget pins the 6000-rune whole-embed cap and
// its order: trailing FIELDS go first, then the description is trimmed, and the
// title is never touched. The title is the only part an operator can identify
// the embed by in a notification popup.
//
// THE MUTANT: trimming the title, or trimming the description before dropping
// fields (the embed then loses its body while keeping 25 near-empty fields).
func TestClampEmbedTrimsToTheTotalBudget(t *testing.T) {
	title := jp(100)
	e := &discordEmbed{Title: title, Description: jp(4000)}
	for range 5 {
		e.Fields = append(e.Fields, discordField{Name: jp(100), Value: jp(1000)})
	}
	// 100 + 4000 + 5*(100+1000) = 9600 runes, well over 6000.
	clampEmbed(e)

	if e.Title != title {
		t.Errorf("the title was trimmed to %q — it is the one part that must survive", e.Title)
	}
	if total := embedRunes(e); total > limitTotal {
		t.Errorf("embed totals %d runes, want <= %d", total, limitTotal)
	}
	if len(e.Fields) == 5 {
		t.Error("no field was dropped — trailing fields are what goes first")
	}
	if !utf8.ValidString(e.Description) {
		t.Error("the description trim split a rune")
	}
}

// TestEscapeMarkdown is the table for the helper producers apply to
// job-supplied text. Per Discord API docs (reference.mdx) embed descriptions
// and field values render Discord's markdown subset, so a stream title with
// `_`, `*`, `~~`, `||`, a backtick or a `<t:…>`-shaped fragment re-renders as
// formatting. Pings are impossible from an embed, so this is formatting only.
//
// THE MUTANT: escaping the backslash LAST instead of first — every other
// escape's own backslash then gets doubled.
func TestEscapeMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain title", "plain title"},
		{"a_b", `a\_b`},
		{"*bold*", `\*bold\*`},
		{"~~strike~~", `\~\~strike\~\~`},
		{"a||b", `a\|\|b`},
		{"`code`", "\\`code\\`"},
		{"<t:123:R>", `\<t:123:R\>`},
		{`back\slash`, `back\\slash`},
		{"# heading", `\# heading`},
		{"- item", `\- item`},
		{"> quote", `\> quote`},
		// # and - are special ONLY at the start of a line (spec §1.3 says
		// "leading"), so a mid-line one stays literal. > is escaped everywhere
		// because it also opens <t:…>, <@…> and <:emoji:id>.
		{"mid # hash", "mid # hash"},
		{"a - b", "a - b"},
		{"line1\n# line2", "line1\n\\# line2"},
		{"\n- second line", "\n\\- second line"},
		{"あ_い", `あ\_い`},
		// A masked link renders in descriptions and field values; escaping
		// the brackets keeps a title's [text](url) literal.
		{"[Claim prize](https://phish.example)", `\[Claim prize\](https://phish.example)`},
	} {
		if got := EscapeMarkdown(tc.in); got != tc.want {
			t.Errorf("EscapeMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseTargetAcceptsTheLegacyDiscordappHost is audit R6. discordapp.com is
// Discord's legacy domain and still serves webhooks; the regex rejected it
// outright, so a webhook pasted from an old bookmark was warn-skipped at
// startup with a message that did not say why.
//
// The resolved URL is CANONICALISED to discord.com, which is what makes the
// dedupe correct: the two spellings of one webhook must collapse to one target
// (buildTargets keys on the resolved URL), and it also avoids a redirect —
// Go's http.Client turns a 301/302 on POST into a GET, silently losing the body.
func TestParseTargetAcceptsTheLegacyDiscordappHost(t *testing.T) {
	const id, tok = "123456789012345678", "tok-en_ABC"
	for _, tc := range []struct {
		in      string
		wantURL string
	}{
		{"https://discordapp.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
		// Every spelling of one webhook resolves to ONE URL (canonicalDiscordURL):
		// ptb./canary. subdomains, a trailing slash, the legacy host — each used
		// to build its own target and post every embed again. The query stays.
		// Mutant: the host or the slash left as given — these rows differ.
		{"https://ptb.discordapp.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
		{"https://canary.discord.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
		{"https://discord.com/api/webhooks/" + id + "/" + tok + "/", "https://discord.com/api/webhooks/" + id + "/" + tok},
		{"https://discord.com/api/webhooks/" + id + "/" + tok + "/?thread_id=9", "https://discord.com/api/webhooks/" + id + "/" + tok + "?thread_id=9"},
		{"https://discord.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
	} {
		s, err := parseTarget(tc.in)
		if err != nil {
			t.Fatalf("parseTarget(%q): %v", tc.in, err)
		}
		d, ok := s.(*DiscordWebhook)
		if !ok {
			t.Fatalf("parseTarget(%q) did not return a DiscordWebhook", tc.in)
		}
		if d.URL != tc.wantURL {
			t.Errorf("parseTarget(%q).URL = %q, want %q", tc.in, d.URL, tc.wantURL)
		}
	}

	t.Run("a malformed discordapp URL is still rejected with the specific message", func(t *testing.T) {
		_, err := parseTarget("http://discordapp.com/api/webhooks/abc/")
		if err == nil || !strings.Contains(err.Error(), "must be HTTPS") {
			t.Errorf("err = %v, want the specific Discord-webhook rejection", err)
		}
	})

	t.Run("the two spellings collapse to one target", func(t *testing.T) {
		cfg := notifConfigWithURLs(
			"https://discordapp.com/api/webhooks/"+id+"/"+tok,
			"https://discord.com/api/webhooks/"+id+"/"+tok,
		)
		if got := len(buildTargets(cfg, testLogger{})); got != 1 {
			t.Errorf("built %d targets, want 1 — the legacy spelling must dedupe against the current one", got)
		}
	})
}

// TestClampEmbedDropsEmptyFields: Discord answers a field with an empty name
// or value with a 400, deliver treats that as permanent, and the whole embed
// is dropped. Trim Failed carried the job's channel straight from the row and
// Trim Deleted its title, so a channel-less job's failure was never delivered.
// The empty field goes; the message stays.
//
// Mutant: dropping the filter — the empty fields survive.
func TestClampEmbedDropsEmptyFields(t *testing.T) {
	e := &discordEmbed{
		Title: "Trim Failed",
		Fields: []discordField{
			{Name: "Channel", Value: ""},
			{Name: "Video ID", Value: "abc"},
			{Name: "", Value: "orphan value"},
			{Name: "Source Video", Value: "   "},
		},
	}
	clampEmbed(e)
	if len(e.Fields) != 1 || e.Fields[0].Name != "Video ID" {
		t.Errorf("fields after clamp = %+v, want only the non-empty Video ID", e.Fields)
	}
}

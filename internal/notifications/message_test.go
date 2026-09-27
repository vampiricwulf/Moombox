package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestOneEmbedMessageIsByteIdenticalToN1 is this task's whole safety
// argument. Every one of the ~36 producer sites goes through the single-embed
// path, and the seam rewrites that path for the sake of a feature none of
// them use. The payload they produce must not move by one byte.
//
// The expected JSON is N1's, captured from buildPayload at 242d921c with the
// timestamp elided (it is time.Now at build time). Field ORDER matters: it is
// struct order in discordPayload/discordEmbed, and a reordering would be a
// silent diff in every operator's channel.
func TestOneEmbedMessageIsByteIdenticalToN1(t *testing.T) {
	opts := SendOptions{
		URL:       "https://youtube.com/watch?v=abc",
		Event:     "finished",
		Thumbnail: "https://i.example/t.jpg",
		Platform:  "YouTube",
		JobID:     "job-1",
		Author:    &Author{Name: "Some Channel", URL: "https://youtube.com/@some", IconURL: "https://i.example/a.jpg"},
	}
	fields := []Field{{Name: "File", Value: "out.mp4"}, {Name: "Duration", Value: "1:02:03", Inline: true}}

	body, err := buildPayload(One("Download Finished", "Successfully archived: X", 0x2ecc71, fields, opts))
	if err != nil {
		t.Fatal(err)
	}
	got := elideTimestamp(t, body)

	const want = `{"embeds":[{"title":"Download Finished","description":"Successfully archived: X",` +
		`"color":3066993,"url":"https://youtube.com/watch?v=abc",` +
		`"author":{"name":"Some Channel","url":"https://youtube.com/@some","icon_url":"https://i.example/a.jpg"},` +
		`"fields":[{"name":"File","value":"out.mp4"},{"name":"Duration","value":"1:02:03","inline":true}],` +
		`"thumbnail":{"url":"https://i.example/t.jpg"},` +
		`"footer":{"text":"Moombox · YouTube · job-1"},"timestamp":"ELIDED"}]}`
	if got != want {
		t.Errorf("a one-embed Message no longer produces N1's payload.\n got: %s\nwant: %s", got, want)
	}
}

// elideTimestamp replaces every embed timestamp with a constant so the
// comparison is about shape, not about the clock.
func elideTimestamp(t *testing.T, body []byte) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	for _, e := range v["embeds"].([]any) {
		if _, ok := e.(map[string]any)["timestamp"]; !ok {
			t.Fatal("an embed carried no timestamp — the elision below would be a no-op and the test vacuous")
		}
	}
	// Re-marshalling through a map would reorder keys alphabetically and lose
	// the point of the test, so edit the raw bytes instead. The scan CONSUMES
	// what it has rewritten: replacing in place and re-searching from the top
	// would find the same "timestamp":"ELIDED" forever.
	const key = `"timestamp":"`
	var out strings.Builder
	rest := string(body)
	for {
		i := strings.Index(rest, key)
		if i < 0 {
			out.WriteString(rest)
			return out.String()
		}
		value := rest[i+len(key):]
		j := strings.Index(value, `"`)
		if j < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:i+len(key)])
		out.WriteString("ELIDED")
		rest = value[j:]
	}
}

// TestMessageCarriesTheMention pins that the content mention moved from the
// embed's opts to the MESSAGE, which is what lets a ten-embed batch ping once.
func TestMessageCarriesTheMention(t *testing.T) {
	msg := One("T", "D", 0x1, nil, SendOptions{})
	msg.Mention = "<@&123456789012345678>"
	msg.MentionAllowed = MentionParse(msg.Mention)

	body, err := buildPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json HTML-escapes <, > and & by default, so the content arrives
	// as "<@&123456789012345678>" on the wire. Compare it
	// DECODED — a raw-substring check would be asserting the escaping, not the
	// mention.
	var p struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Content != msg.Mention {
		t.Errorf("the message content did not carry the mention: %s", body)
	}
	if !strings.Contains(string(body), `"allowed_mentions":{"parse":[],"roles":["123456789012345678"]}`) {
		t.Errorf("allowed_mentions is not the object MentionParse built: %s", body)
	}
}

// TestBuildPayloadClampsEveryEmbed pins that the PAYLOAD path clamps — EVERY
// embed of it, not just the first.
//
// limits_test.go calls clampEmbed directly, so it cannot see the clamp leaving
// the path that actually posts: over ANY Discord limit is a 400, the retry
// ladder treats a non-429 4xx as permanent, and an unclamped embed is
// therefore a silently dropped alert. That gap was survivable while the clamp
// sat inline in buildPayload; now that it lives in toDiscordEmbed, which the
// batcher calls too, nothing else would notice it going missing.
func TestBuildPayloadClampsEveryEmbed(t *testing.T) {
	long := strings.Repeat("x", limitTitle+50)
	msg := Message{Embeds: []Embed{
		{Title: long, Opts: SendOptions{Event: "found"}},
		{Title: long, Opts: SendOptions{Event: "found"}},
	}}

	body, err := buildPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Embeds []struct {
			Title string `json:"title"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embeds) != 2 {
		t.Fatalf("payload carried %d embeds, want 2", len(v.Embeds))
	}
	for i, e := range v.Embeds {
		if n := utf8.RuneCountInString(e.Title); n != limitTitle {
			t.Errorf("embed %d's title is %d characters, want %d — the payload path did not clamp it, "+
				"and Discord answers an over-long embed with a permanent 400", i, n, limitTitle)
		}
	}
}

// TestMultiEmbedMessageEmitsEveryEmbed is the capability the seam exists for.
func TestMultiEmbedMessageEmitsEveryEmbed(t *testing.T) {
	msg := Message{}
	for i := range 3 {
		msg.Embeds = append(msg.Embeds, Embed{
			Title: "Stream Found " + itoa(i),
			Opts:  SendOptions{Event: "found", JobID: "j" + itoa(i)},
		})
	}
	body, err := buildPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Embeds []struct {
			Title  string `json:"title"`
			Footer struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embeds) != 3 {
		t.Fatalf("payload carried %d embeds, want 3", len(v.Embeds))
	}
	for i, e := range v.Embeds {
		if e.Title != "Stream Found "+itoa(i) {
			t.Errorf("embed %d out of order: %q", i, e.Title)
		}
		if !strings.HasSuffix(e.Footer.Text, "j"+itoa(i)) {
			t.Errorf("embed %d's footer is not its own (%q) — Opts must be PER EMBED, or a batch of "+
				"ten jobs would show the same job id ten times", i, e.Footer.Text)
		}
	}
}

package notifications

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A webhook URL's query is canonical too. buildTargets dedupes on the resolved
// URL, so the same parameters in another order, a stray '&', or a
// "?wait=true" — which the sender adds itself to the one request that wants
// it (execWaitURL) and strips from the others (messageURL) — each built a
// second target over one webhook, and every embed posted twice. The query is
// now the parameters sorted by name, as url.Values encodes them, with every
// nameless one and every wait dropped; thread_id and any other parameter
// stays, and a query net/url refuses to parse is kept as given.
//
// Mutants: canonicalDiscordURL keeping the query as given — every reordered,
// '&' and wait row fails; keeping wait — the wait rows fail; keeping a
// nameless pair — the "=x" row fails; dropping a named parameter with an
// empty value — the "thread_id=" row fails; dropping the pairs a parse
// refused — the ';' row fails.
func TestAWebhooksQueryIsCanonical(t *testing.T) {
	const id, tok = "123456789012345678", "tok-en_ABC"
	base := "https://discord.com/api/webhooks/" + id + "/" + tok
	for _, tc := range []struct{ in, want string }{
		{base + "?thread_id=9", base + "?thread_id=9"},
		{base + "?thread_id=9&", base + "?thread_id=9"},
		{base + "?&thread_id=9", base + "?thread_id=9"},
		{base + "?thread_id=9&&with_components=true", base + "?thread_id=9&with_components=true"},
		{base + "?with_components=true&thread_id=9", base + "?thread_id=9&with_components=true"},
		{base + "?=x&thread_id=9", base + "?thread_id=9"},
		{base + "?wait=true", base},
		{base + "/?wait=true&", base},
		{base + "?wait=true&thread_id=9", base + "?thread_id=9"},
		{base + "?thread_id=9&wait=false", base + "?thread_id=9"},
		{"discord://" + id + "/" + tok + "?wait=true&thread_id=9", base + "?thread_id=9"},
		{"https://ptb.discordapp.com/api/webhooks/" + id + "/" + tok + "/?thread_id=9&", base + "?thread_id=9"},
		// A named parameter with no value is not nothing: folded into the
		// bare URL, it would post to the channel what was configured for a
		// thread.
		{base + "?thread_id=", base + "?thread_id="},
		// Two values of one parameter keep their order: which one the server
		// reads is its business.
		{base + "?thread_id=2&thread_id=1", base + "?thread_id=2&thread_id=1"},
		// net/url refuses a ';' separator, and the pair it skipped is still
		// a parameter of the URL the operator configured.
		{base + "?b=1;a=2", base + "?b=1;a=2"},
	} {
		s, err := parseTarget(tc.in)
		if err != nil {
			t.Fatalf("parseTarget(%q): %v", tc.in, err)
		}
		if got := s.(*DiscordWebhook).URL; got != tc.want {
			t.Errorf("parseTarget(%q).URL = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every spelling of one webhook's query is one target, and two queries that
// mean two destinations stay two.
//
// Mutant: canonicalDiscordURL keeping the query as given — the first two rows
// build more than one target.
func TestAWebhooksQuerySpellingsAreOneTarget(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	base := "https://discord.com/api/webhooks/" + id + "/" + tok
	for _, tc := range []struct {
		name string
		urls []string
		want int
	}{
		{"one thread, every spelling", []string{
			base + "?thread_id=9",
			base + "?thread_id=9&",
			base + "?wait=true&thread_id=9",
			"discord://" + id + "/" + tok + "?thread_id=9&wait=true",
		}, 1},
		{"wait is no destination", []string{base, base + "?wait=true", base + "/?wait=true&"}, 1},
		{"two threads", []string{base + "?thread_id=9", base + "?thread_id=10"}, 2},
		{"a thread and its channel", []string{base, base + "?thread_id=9"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MoomboxConfig{}
			for _, u := range tc.urls {
				cfg.Notifications = append(cfg.Notifications, config.NotificationConfig{URL: u})
			}
			if got := len(buildTargets(cfg, testLogger{})); got != tc.want {
				t.Errorf("built %d targets, want %d", got, tc.want)
			}
		})
	}
}

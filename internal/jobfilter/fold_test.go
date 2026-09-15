package jobfilter

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// containsFoldLower must be indistinguishable from what it replaces:
// strings.Contains(strings.ToLower(s), sub). Each row asserts the literal
// expectation AND that the baseline expression agrees, so the table itself
// cannot drift away from the twin's rule.
func TestContainsFoldLowerMatchesToLowerContains(t *testing.T) {
	for _, tc := range []struct {
		name, s, sub string
		want         bool
	}{
		{"ascii hit", "Stream 42 KARAOKE night", "karaoke", true},
		{"ascii miss", "Stream 42 karaoke night", "jazz", false},
		{"empty needle matches everything", "anything", "", true},
		{"empty haystack", "", "a", false},
		{"needle longer than haystack", "ab", "abc", false},
		{"backtracking prefix", "aaab", "ab", true},
		{"accented uppercase folds down", "ÉCOLE du soir", "école", true},
		{"accented needle alone", "école", "é", true},
		{"kelvin sign lowers to k", "Kelvin scale", "kelvin", true},
		// Kills the fold-EQUIVALENCE mutant — comparing with an
		// EqualFold-style orbit relation instead of ToLower equality, which
		// reports "ſun" and "sun" as the same string where the dashboard
		// (String.prototype.toLowerCase) reports two different ones. It does
		// NOT kill a literal unicode.SimpleFold substitution inside
		// lowerRune: SimpleFold('ſ') returns 'S', not 's', so that mutant
		// still fails to match "sun" on this row — it dies on the
		// "accented needle alone" row below, where SimpleFold('é') returns
		// 'É' and the needle no longer matches itself.
		{"long s is not an s", "ſun", "sun", false},
		{"dotted capital I lowers to i", "İstanbul", "i", true},
		{"dotless i is not an i", "ıstanbul", "i", false},
		{"cjk passthrough", "配信アーカイブ", "アーカイブ", true},
		{"invalid utf-8 becomes the replacement rune", "a\xffb", "�", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if baseline := strings.Contains(strings.ToLower(tc.s), tc.sub); baseline != tc.want {
				t.Fatalf("the table disagrees with the semantics it mirrors: "+
					"Contains(ToLower(%q), %q) = %v, table says %v", tc.s, tc.sub, baseline, tc.want)
			}
			if got := containsFoldLower(tc.s, tc.sub); got != tc.want {
				t.Errorf("containsFoldLower(%q, %q) = %v, want %v", tc.s, tc.sub, got, tc.want)
			}
		})
	}
}

// The user-visible half: a non-ASCII query still matches a differently-cased
// title, channel or video ID.
func TestTextTermFoldsNonASCII(t *testing.T) {
	job := &database.Job{ID: "j", Title: "ÉCOLE du soir", ChannelName: "Kelvin", VideoID: "dQw4w9WgXcQ", Status: database.StatusFinished, Platform: "youtube"}
	for _, q := range []string{"école", "kelvin", "W9WG"} {
		if !Match(Parse(q), job) {
			t.Errorf("query %q must match %+v", q, job)
		}
	}
	if Match(Parse("jazz"), job) {
		t.Error("an unrelated query must not match")
	}
}

// The point of the change. Mutant: any strings.ToLower left in matchTerm's
// text arm — that is one allocation per job per match, 1000 allocs/op at 1000
// jobs, run on every keystroke in the / box.
func TestTextMatchAllocatesNothing(t *testing.T) {
	job := &database.Job{ID: "j", Title: "Stream 42 karaoke night", ChannelName: "Channel 7", VideoID: "vid00000042", Status: database.StatusFinished, Platform: "youtube"}
	ascii := Parse("karaoke")
	nonASCII := Parse("école")
	accented := &database.Job{ID: "k", Title: "ÉCOLE du soir", ChannelName: "Kelvin", VideoID: "vid1"}

	if got := testing.AllocsPerRun(200, func() { Match(ascii, job) }); got != 0 {
		t.Errorf("an ASCII text match allocated %v times per run, want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() { Match(nonASCII, accented) }); got != 0 {
		t.Errorf("a non-ASCII text match allocated %v times per run, want 0", got)
	}
}

// equalFoldLower must be indistinguishable from strings.ToLower(s) == sub —
// the relation the dashboard's === on two toLowerCase values implements, and
// the one the text arm already uses. Each row asserts the baseline agrees, as
// the containsFoldLower table does, so the table cannot drift off the twin.
//
// Mutant: restoring strings.EqualFold (what the status, channel and platform
// arms used) — the SimpleFold ORBIT relation, which reports "ſun" and "sun"
// as the same string.
func TestEqualFoldLowerMatchesToLowerEquality(t *testing.T) {
	for _, tc := range []struct {
		name, s, sub string
		want         bool
	}{
		{"exact lower", "sun", "sun", true},
		{"cased haystack", "Sun", "sun", true},
		{"all caps", "SUN", "sun", true},
		{"different word", "moon", "sun", false},
		{"trailing space is not trimmed", "sun ", "sun", false},
		{"leading space is not trimmed", " sun", "sun", false},
		{"substring is not equality", "sunset", "sun", false},
		{"prefix is not equality", "su", "sun", false},
		{"both empty", "", "", true},
		{"empty needle rejects a non-empty value", "sun", "", false},
		{"empty haystack rejects a needle", "", "sun", false},
		// Mutant: restoring strings.EqualFold — EqualFold("ſun", "sun") is
		// true (U+017F shares 's's fold orbit) where ToLower equality, and
		// the dashboard, say false.
		{"long s is not an s", "ſun", "sun", false},
		// The other direction: the KELVIN SIGN really does lower-case to
		// 'k', in Go and in JS, so this one must stay true.
		{"kelvin sign lowers to k", "\u212AELVIN", "kelvin", true},
		{"accented", "ÉCOLE", "école", true},
		{"cjk passthrough", "配信アーカイブ", "配信アーカイブ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if baseline := strings.ToLower(tc.s) == tc.sub; baseline != tc.want {
				t.Fatalf("the table disagrees with the semantics it mirrors: "+
					"ToLower(%q) == %q is %v, table says %v", tc.s, tc.sub, baseline, tc.want)
			}
			if got := equalFoldLower(tc.s, tc.sub); got != tc.want {
				t.Errorf("equalFoldLower(%q, %q) = %v, want %v", tc.s, tc.sub, got, tc.want)
			}
		})
	}
}

// The user-visible half of the same rule: channel:, platform: and a raw
// status: name compare exactly what the dashboard compares.
//
// Mutant: restoring strings.EqualFold in any of the three arms — the TUI
// would then match "ſun" against channel "sun" and the dashboard would not,
// so the same query would list different jobs in the two UIs.
func TestNamespaceArmsUseToLowerEquality(t *testing.T) {
	job := &database.Job{ID: "j", Title: "Stream", ChannelName: "sun", Platform: "youtube", Status: database.StatusFinished}

	// The U+017F probe needs an 's' in the value, so it lands on channel: and
	// status:. platform: shares the same helper and is covered below by the
	// case and non-containment rows — no realistic platform name carries a
	// character whose ToLower and fold relations disagree (the two that do,
	// U+0130 and the final sigma, are the Go-vs-JS points that are out of
	// scope for both twins).
	for _, q := range []string{"channel:ſun", "status:finiſhed"} {
		if Match(Parse(q), job) {
			t.Errorf("query %q must not match %+v — U+017F is not an 's' to ToLower, and the dashboard does not match it either", q, job)
		}
	}
	for _, q := range []string{"channel:SUN", "channel:sun", "platform:YouTube", "status:FINISHED"} {
		if !Match(Parse(q), job) {
			t.Errorf("query %q must match %+v — case is the only thing folded", q, job)
		}
	}
	// Equality, not containment: the arms must not become substring matches.
	for _, q := range []string{"channel:su", "platform:you"} {
		if Match(Parse(q), job) {
			t.Errorf("query %q must not match %+v — the namespace arms compare whole values", q, job)
		}
	}
}

// The namespace arms run over every job on every keystroke exactly as the
// text arm does. Mutant: writing them as strings.ToLower(field) == t.lower —
// one allocation per job per match, 1000 allocs/op at 1000 jobs.
func TestNamespaceMatchAllocatesNothing(t *testing.T) {
	job := &database.Job{ID: "j", Title: "Stream", ChannelName: "Channel 7", Platform: "youtube", Status: database.StatusFinished}
	channel := Parse("channel:channel 7")
	platform := Parse("platform:youtube")
	status := Parse("status:finished")

	if got := testing.AllocsPerRun(200, func() { Match(channel, job) }); got != 0 {
		t.Errorf("a channel: match allocated %v times per run, want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() { Match(platform, job) }); got != 0 {
		t.Errorf("a platform: match allocated %v times per run, want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() { Match(status, job) }); got != 0 {
		t.Errorf("a raw status: match allocated %v times per run, want 0", got)
	}
}

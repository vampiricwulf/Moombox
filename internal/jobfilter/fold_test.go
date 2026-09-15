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
		// The SimpleFold mutant killer: U+017F folds to 's' under
		// unicode.SimpleFold but lower-cases to itself, and the dashboard
		// (String.prototype.toLowerCase) does not match it either.
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

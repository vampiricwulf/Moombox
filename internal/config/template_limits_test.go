package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A Japanese title keeps three bytes a character through the sanitizer, and
// Linux caps a file name at 255 bytes: a 90-character title made
// "<title> [<id>].mp4" impossible to create, so the finalize failed with
// ENAMETOOLONG. ResolveTemplate now caps the title's bytes, on a rune boundary,
// leaving room for the id and the longest suffix written beside an archive.
// ASCII titles, which no platform lets reach the cap, come through whole.
//
// Mutant: the title not truncated — the CJK name is over the limit.
func TestResolveTemplateKeepsNamesWithinTheFilesystemLimit(t *testing.T) {
	title := strings.Repeat("あ", 100)
	got := ResolveTemplate("${title} [${id}]", TemplateVariables{Title: title, ID: "tw_123456789012345"})
	longest := got + " - part99.restart-1700000000-9.chat.json"
	if len(longest) > 255 {
		t.Errorf("a 100-character CJK title resolves to %d bytes; with the longest suffix that is %d, over 255",
			len(got), len(longest))
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, " [tw_123456789012345]") {
		t.Errorf("resolved %q — the cut must fall on a rune boundary and keep the id", got)
	}

	ascii := strings.Repeat("a", 140)
	if got := ResolveTemplate("${title}", TemplateVariables{Title: ascii}); got != ascii {
		t.Errorf("a 140-character ASCII title was cut to %d bytes", len(got))
	}
}

// A channel whose sanitized name is a Windows device name made the default
// "${channel}/..." layout's MkdirAll fail at every finalize on Windows, and a
// title that is one did the same to the file. Every such component gets an
// underscore, extension or not.
//
// Mutant: guardReservedComponents returning its input unchanged.
func TestResolveTemplateGuardsWindowsDeviceNames(t *testing.T) {
	for _, tc := range []struct {
		template string
		vars     TemplateVariables
		want     string
	}{
		{"${channel}/${title} [${id}]", TemplateVariables{Channel: "CON", Title: "Stream", ID: "x"}, "_CON/Stream [x]"},
		{"${title}", TemplateVariables{Title: "nul"}, "_nul"},
		{"${channel}\\${title}", TemplateVariables{Channel: "Com1", Title: "AUX"}, "_Com1\\_AUX"},
		{"${channel}/${title}", TemplateVariables{Channel: "Console", Title: "Lpt10"}, "Console/Lpt10"},
	} {
		if got := ResolveTemplate(tc.template, tc.vars); got != tc.want {
			t.Errorf("ResolveTemplate(%q, %+v) = %q, want %q", tc.template, tc.vars, got, tc.want)
		}
	}
}

// The title and channel caps fit the default layout only. A template that
// puts both in one component — "${channel} - ${title} [${id}]" — resolved to
// about 400 bytes and the finalize failed with ENAMETOOLONG. Each component
// now fits: the free text shrinks, level between the two, and the id stays.
//
// Mutants: fitTemplateComponents returning the plain resolution (the name is
// over); shrinkFreeText ignoring the other value (the channel is cut to the
// floor while the title keeps 150 bytes).
func TestResolveTemplateFitsEveryComponent(t *testing.T) {
	channel := strings.Repeat("c", 200)
	title := strings.Repeat("あ", 60) // 180 bytes
	const id = "tw_123456789012345"
	got := ResolveTemplate("${channel} - ${title} [${id}]", TemplateVariables{Channel: channel, Title: title, ID: id})
	if len(got) > templateStemMaxBytes {
		t.Errorf("resolved to %d bytes, over the %d an archive name may use", len(got), templateStemMaxBytes)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, " ["+id+"]") {
		t.Errorf("resolved %q — the cut must fall on a rune boundary and keep the id", got)
	}
	gotChannel, rest, _ := strings.Cut(got, " - ")
	gotTitle := strings.TrimSuffix(rest, " ["+id+"]")
	if d := len(gotChannel) - len(gotTitle); d < -3 || d > 3 {
		t.Errorf("channel %d bytes, title %d — the two should end up level", len(gotChannel), len(gotTitle))
	}

	// A directory gets a whole name.
	dir := ResolveTemplate("${channel} ${channel}/${title}", TemplateVariables{Channel: channel, Title: "t"})
	first, last, _ := strings.Cut(dir, "/")
	if len(first) > templateNameMaxBytes || last != "t" {
		t.Errorf("directory %d bytes, file %q — want at most %d and the title untouched", len(first), last, templateNameMaxBytes)
	}

	// Literal text cannot shrink, so as a last resort the component is cut.
	if got := ResolveTemplate(strings.Repeat("x", 300), TemplateVariables{}); len(got) != templateStemMaxBytes {
		t.Errorf("a 300-byte literal name resolved to %d bytes, want %d", len(got), templateStemMaxBytes)
	}
}

// The default layout at its caps — a 180-byte title, an 18-byte Twitch id —
// is the budget the suffix reserve was sized against, and resolves untouched.
func TestTheDefaultTemplateAtItsCapsIsNotShrunk(t *testing.T) {
	title := strings.Repeat("あ", 60)
	got := ResolveTemplate(Defaults().Downloader.OutputTemplate,
		TemplateVariables{Channel: strings.Repeat("c", 200), Title: title, ID: "tw_123456789012345"})
	if !strings.Contains(got, " "+title+" [") {
		t.Errorf("the default template shrank a title at the cap: %q", got)
	}
}

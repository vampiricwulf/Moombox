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

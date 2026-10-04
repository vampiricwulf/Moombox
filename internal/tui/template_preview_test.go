package tui

import (
	"regexp"
	"testing"
)

// TestTemplatePreviewMatchesTheResolver: the preview formatted ${start_date}
// as 2006-01-02 and ${start_time} as 20-00-00 and appended .mkv, while
// config.ResolveTemplate writes 20060102 and 1504 and the muxer writes .mp4,
// so the example was a name no recording ever got.
//
// Mutant: restore the hand-rolled replacer — the dashed date and .mkv fail.
func TestTemplatePreviewMatchesTheResolver(t *testing.T) {
	got := templatePreview("${channel}/${start_date} ${title} [${id}] ${start_time}")
	want := regexp.MustCompile(`^Example: Miko Ch/\d{8} Singing Stream \[dQw4w9WgXcQ\] 2000\.mp4$`)
	if !want.MatchString(got) {
		t.Errorf("preview = %q, want the resolver's formats (YYYYMMDD, HHMM, .mp4)", got)
	}
}

package config

import (
	"strings"
	"testing"
)

// The 500-byte output_template cap lived in the web PUT alone: the TUI saved
// a longer template and a hand-edited file loaded one. It is part of Validate
// now, so every writer and every load apply the same rule.
//
// Mutant: the cap dropped from validateOrNormalize — the long template
// validates.
func TestValidateCapsTheOutputTemplate(t *testing.T) {
	cfg := Defaults()
	cfg.Downloader.OutputTemplate = strings.Repeat("x", OutputTemplateMaxLen)
	if errs := Validate(cfg); len(errs) != 0 {
		t.Fatalf("a %d-byte template was refused: %v", OutputTemplateMaxLen, errs)
	}
	cfg.Downloader.OutputTemplate += "x"
	errs := Validate(cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "output_template") {
		t.Errorf("Validate = %v, want the output_template cap", errs)
	}
}

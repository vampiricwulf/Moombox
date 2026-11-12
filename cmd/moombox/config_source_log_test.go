package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// configSourceLogger captures Info lines with their attributes so a test can
// assert on the path string a boot line actually carries.
type configSourceLogger struct {
	lines []string
}

func (l *configSourceLogger) Debug(string, ...any) {}
func (l *configSourceLogger) Warn(string, ...any)  {}
func (l *configSourceLogger) Error(string, ...any) {}
func (l *configSourceLogger) Info(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		if attr, ok := a.(slog.Attr); ok {
			b.WriteString(" " + attr.Key + "=" + attr.Value.String())
			continue
		}
		b.WriteString(" ?")
	}
	l.lines = append(l.lines, b.String())
}

func (l *configSourceLogger) joined() string { return strings.Join(l.lines, "\n") }

// O-Y made an explicit -config path authoritative, which closed the
// wrong-file-adopted bug and opened a quieter one: `-config C:/mooombox.toml`
// (a typo) no longer finds anything, the run starts on defaults, and the first
// save creates a SECOND config beside the intended one — with nothing in the
// log naming either path. The boot now says which file answered, or which one
// a save will create.
//
// Mutant: deleting the logConfigSource call / the log line — the captured
// logger holds no path at all and both halves below fail.
func TestLogConfigSourceNamesThePath(t *testing.T) {
	t.Run("loaded", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.LoadedFrom = "C:/moombox/config.toml"
		cfg.ConfigLoaded = true
		l := &configSourceLogger{}

		logConfigSource(l, cfg, "C:/elsewhere/config.toml")

		got := l.joined()
		if !strings.Contains(got, cfg.LoadedFrom) {
			t.Errorf("boot log does not name the file that was read (%q); got:\n%s", cfg.LoadedFrom, got)
		}
		if strings.Contains(got, "elsewhere") {
			t.Errorf("boot log names the path that was ASKED for, not the one that answered; got:\n%s", got)
		}
	})

	t.Run("nothing on disk", func(t *testing.T) {
		cfg := config.Defaults() // LoadedFrom "", ConfigLoaded false
		target := "C:/typo/mooombox.toml"
		l := &configSourceLogger{}

		logConfigSource(l, cfg, target)

		got := l.joined()
		if !strings.Contains(got, target) {
			t.Errorf("boot log does not name the path a save will create (%q); got:\n%s", target, got)
		}
		// The operator has to be able to tell the two apart — a typo'd
		// -config must not read like a successful load.
		if !strings.Contains(strings.ToLower(got), "default") {
			t.Errorf("the no-file line does not say the run is on defaults; got:\n%s", got)
		}
	})
}

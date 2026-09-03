package cookies

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// levelTaggedLogger records the LEVEL with the message and the args, which is
// what these three tests are about: the same verdict has to arrive at two
// different levels depending on the acquisition mode, and a logger that keeps
// only the text cannot tell the fix from the defect.
//
// Its own type rather than capturingLogger next door: that one folds Error into
// msgs without a level-specific slice, so "logged at ERROR" is not assertable
// through it.
type levelTaggedLogger struct {
	lines []loggedLine
}

type loggedLine struct {
	level string
	msg   string
	args  []any
}

func (l *levelTaggedLogger) record(level, msg string, args ...any) {
	l.lines = append(l.lines, loggedLine{level: level, msg: msg, args: args})
}

func (l *levelTaggedLogger) Debug(msg string, args ...any) { l.record("debug", msg, args...) }
func (l *levelTaggedLogger) Info(msg string, args ...any)  { l.record("info", msg, args...) }
func (l *levelTaggedLogger) Warn(msg string, args ...any)  { l.record("warn", msg, args...) }
func (l *levelTaggedLogger) Error(msg string, args ...any) { l.record("error", msg, args...) }

// at returns every line logged at one level, rendered as message plus args, so
// an assertion about the CONTENT reads the whole line and not just its heading.
// The guard's refusal travels as an error ARG, not in the message, so a helper
// that returned msg alone could not see it.
func (l *levelTaggedLogger) at(level string) []string {
	var out []string
	for _, ln := range l.lines {
		if ln.level != level {
			continue
		}
		out = append(out, ln.msg+" "+fmt.Sprint(ln.args...))
	}
	return out
}

// TestProfileDirVerdictIsSilentAtConstruction is F1's first half.
//
// NewAutoCookieService cannot know the acquisition mode: cmd/moombox builds the
// service and only then wires AcquisitionMode, so an ERROR chosen in the
// constructor is chosen blind — and on the configuration the README recipe
// prescribes (cookies.acquisition = "profile" pointed at a REAL profile) it was
// wrong on every boot. The verdict is still computed here; the sentence is not
// said here.
//
// Zero lines AT ANY LEVEL, not "no error line": a Warn or an Info emitted from
// the constructor would be the same defect at a quieter volume, and the mode is
// no more knowable for it.
//
// Mutation: put the `if profileDirErr != nil && logger != nil { logger.Error(...) }`
// block back in NewAutoCookieService.
func TestProfileDirVerdictIsSilentAtConstruction(t *testing.T) {
	log := &levelTaggedLogger{}
	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)

	if s.profileDirErr == nil {
		t.Fatal("premise broken: the fixture is not a refused profile dir, so nothing would be logged anyway")
	}
	if len(log.lines) != 0 {
		t.Errorf("the constructor logged %d line(s) about a verdict it cannot level correctly: %v",
			len(log.lines), log.lines)
	}
}

// TestProfileDirVerdictLevelFollowsTheMode is F1's second half: the same
// verdict, two levels, chosen where the mode is finally knowable.
//
// Under "auto" a refused directory means a browser refresh the operator expects
// will silently not happen — ERROR, wording unchanged, because "refusing to
// launch a headless session against it" is the cue. Under "profile" nothing was
// going to launch, the read-only import runs regardless, and an ERROR names a
// failure that did not occur — one INFO that says both halves instead.
//
// Each half asserts the level AND the content, because either alone passes a
// mutant: a right-level line saying the wrong thing, or the right sentence at
// the wrong level. The auto half also pins today's line VERBATIM — message
// and both args — because the arc-close asked only for the profile case to
// change, and a Contains on the refusal wording would pass a reworded heading.
func TestProfileDirVerdictLevelFollowsTheMode(t *testing.T) {
	t.Run("auto", func(t *testing.T) {
		log := &levelTaggedLogger{}
		s := NewAutoCookieService(dangerousProfileDir,
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
		s.AcquisitionMode = func() string { return AcquisitionAuto }

		s.LogProfileDirVerdict()

		errs := log.at("error")
		if len(errs) != 1 {
			t.Fatalf("want exactly one ERROR line under auto, got %d: %v", len(errs), errs)
		}
		if !strings.Contains(errs[0], "refusing to launch") {
			t.Errorf("the auto line dropped the guard's own refusal wording, which is the operator's "+
				"cue that a launch they expect will not happen: %q", errs[0])
		}
		if got := len(log.at("info")); got != 0 {
			t.Errorf("auto also logged %d INFO line(s); the verdict is said once, at one level", got)
		}
		// Today's line, byte for byte: the message, the "err" key and the
		// guard's own error value. The Contains above is the operator's cue;
		// this is the claim that nothing under auto changed at all.
		for _, ln := range log.lines {
			if ln.level != "error" {
				continue
			}
			if ln.msg != "auto-cookie profile dir rejected at construction" || len(ln.args) != 2 ||
				ln.args[0] != "err" || ln.args[1] != any(s.profileDirErr) {
				t.Errorf("the auto line is not today's line verbatim: msg=%q args=%v", ln.msg, ln.args)
			}
		}
	})

	t.Run("profile", func(t *testing.T) {
		log := &levelTaggedLogger{}
		s := NewAutoCookieService(dangerousProfileDir,
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
		s.AcquisitionMode = func() string { return AcquisitionProfile }

		s.LogProfileDirVerdict()

		if got := log.at("error"); len(got) != 0 {
			t.Errorf("the README-prescribed configuration still logs at ERROR on every boot: %v", got)
		}
		if got := log.at("warn"); len(got) != 0 {
			t.Errorf("the profile line was downgraded to WARN rather than stated as the normal case: %v", got)
		}
		infos := log.at("info")
		if len(infos) != 1 {
			t.Fatalf("want exactly one INFO line under profile, got %d: %v", len(infos), infos)
		}
		if strings.Contains(infos[0], "refusing to launch") {
			t.Errorf("the profile line claims a refused launch, on a path that launches nothing: %q", infos[0])
		}
		for _, want := range []string{"no headless browser will be launched", "read-only import"} {
			if !strings.Contains(infos[0], want) {
				t.Errorf("the profile line does not say %q, so it does not tell the operator which "+
					"mechanism actually runs: %q", want, infos[0])
			}
		}
	})
}

// TestProfileDirVerdictSaysNothingForAnAcceptableDir is the premise for both
// tests above: the line is about a REFUSED directory, and an ordinary install
// — which is nearly every install — must see nothing at all.
//
// Mutation: drop the `s.profileDirErr == nil` guard from LogProfileDirVerdict.
func TestProfileDirVerdictSaysNothingForAnAcceptableDir(t *testing.T) {
	for _, mode := range []string{AcquisitionAuto, AcquisitionProfile} {
		t.Run(mode, func(t *testing.T) {
			log := &levelTaggedLogger{}
			s := NewAutoCookieService(t.TempDir(),
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
			s.AcquisitionMode = func() string { return mode }
			if s.profileDirErr != nil {
				t.Fatalf("premise broken: a plain temp dir was refused: %v", s.profileDirErr)
			}

			s.LogProfileDirVerdict()

			if len(log.lines) != 0 {
				t.Errorf("an ordinary profile dir produced %d boot line(s): %v", len(log.lines), log.lines)
			}
		})
	}
}

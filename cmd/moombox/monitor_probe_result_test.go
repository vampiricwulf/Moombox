package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Every field the monitors read reaches them, from both probes (one
// adapter serves both). BroadcastEnded is the one that matters most: it is
// what stops an ended broadcast being announced as upcoming every cycle,
// and a dropped copy would silently bring that loop back.
func TestMonitorProbeResultCarriesEveryField(t *testing.T) {
	got := monitorProbeResult(&youtube.VideoInfo{
		StreamStatus:       youtube.StreamUpcoming,
		Title:              "t",
		PublishedAt:        "2026-09-11T02:14:17Z",
		PublishedPrecision: "started",
		PlayabilityError:   youtube.PlayabilityAgeRestricted,
		BroadcastEnded:     true,
	})
	want := &monitor.VideoProbeResult{
		StreamStatus:       "upcoming",
		Title:              "t",
		PublishedAt:        "2026-09-11T02:14:17Z",
		PublishedPrecision: "started",
		PlayabilityError:   "age_restricted",
		BroadcastEnded:     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("monitorProbeResult = %+v, want %+v", got, want)
	}
	// A field added to VideoProbeResult must be added here, and to the
	// adapter: every one is set above, so a zero one means it was missed.
	v := reflect.ValueOf(*got)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("VideoProbeResult.%s is not carried by monitorProbeResult", v.Type().Field(i).Name)
		}
	}
}

// monitorProbe is the call, then the adapter: what the youtube probe
// answered reaches the monitor whole, and a failed probe reaches it as the
// failure, never as a result.
//
// Mutants: monitorProbe returning a struct literal of the five pre-flag
// fields in place of monitorProbeResult(meta) (BroadcastEnded lost); the
// error swallowed (`return monitorProbeResult(meta), nil` ahead of the
// check — the nil meta panics, or a result arrives for a failed probe).
func TestMonitorProbeIsTheCallThenTheAdapter(t *testing.T) {
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, Title: "t",
		PlayabilityError: youtube.PlayabilityAgeRestricted, BroadcastEnded: true}
	var asked string
	got, err := monitorProbe(func(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
		asked = videoID
		return info, nil
	})(context.Background(), "vid")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if asked != "vid" {
		t.Errorf("the youtube probe was asked about %q, want vid", asked)
	}
	if want := monitorProbeResult(info); !reflect.DeepEqual(got, want) {
		t.Errorf("monitorProbe = %+v, want monitorProbeResult's %+v", got, want)
	}

	boom := errors.New("player: transport error")
	got, err = monitorProbe(func(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
		return nil, boom
	})(context.Background(), "vid")
	if !errors.Is(err, boom) || got != nil {
		t.Errorf("a failed probe gave (%+v, %v), want (nil, %v)", got, err, boom)
	}
}

// Every monitor probe in production is monitorProbe over a youtube.Service
// probe — the anonymous ProbeVideoStatus for both monitors, the
// authenticated one for the feed's membership rows — and nothing else in the
// package assigns one. TestMonitorProbeIsTheCallThenTheAdapter covers what
// monitorProbe does; what puts it in production is these assignments, which
// only a fully built runState reaches, so they are read from source in the
// idiom of TestTheTUIUpdateKeysAreTheTagCheckedHelpers.
//
// Mutants: the ProbeVideoAuth assignment as the pre-flag inline closure
// returning a five-field VideoProbeResult (membership rows lose
// BroadcastEnded, and a member's ended members-only stream whose formats are
// withheld is announced every cycle again — the closure survived the whole
// suite before this test); either monitor's ProbeVideo as an inline closure;
// ProbeVideoAuth built over ProbeVideoStatus (the anonymous client, which
// sees no members-only formats); any of the three assignments deleted.
func TestEveryMonitorProbeIsBuiltByMonitorProbe(t *testing.T) {
	want := map[string]string{
		"s.feedMon.ProbeVideo":     "monitorProbe(s.ytService.ProbeVideoStatus)",
		"s.decapiMon.ProbeVideo":   "monitorProbe(s.ytService.ProbeVideoStatus)",
		"s.feedMon.ProbeVideoAuth": "monitorProbe(s.ytService.ProbeVideoStatusAuthenticated)",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "ProbeVideo" && sel.Sel.Name != "ProbeVideoAuth") {
					continue
				}
				name := types.ExprString(lhs)
				seen[name]++
				w, known := want[name]
				if !known {
					t.Errorf("%s assigns %s, which no monitor probe pin covers", fset.Position(as.Pos()), name)
					continue
				}
				if i >= len(as.Rhs) {
					t.Errorf("%s assigns %s from a multi-value expression, want %s", fset.Position(as.Pos()), name, w)
					continue
				}
				if got := types.ExprString(as.Rhs[i]); got != w {
					t.Errorf("%s sets %s to %s, want %s", fset.Position(as.Pos()), name, got, w)
				}
			}
			return true
		})
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("the package sets %s %d times, want once", name, seen[name])
		}
	}
}

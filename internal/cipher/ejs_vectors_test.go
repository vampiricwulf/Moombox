package cipher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ejsVectorsFile holds the expected answers the Go port of ejs is checked
// against (owner ruling RV-12): upstream ejs's own test pairs (its
// src/yt/solver/test/tests.ts, at the vendored pin c47fb1b and at 6f8587b,
// whose players the newer file no longer lists), and pairs recorded from the
// sidecar's ejs on the cb017549 fixture, which no upstream file covers. It is
// committed; the player bodies are not (YouTube's code): they are fetched by
// .github/scripts/fetch-ejs-vector-players.sh into ejsVectorPlayersDir.
const ejsVectorsFile = "ejs_vectors.json"

// ejsVectorPlayersDir is where fetch-ejs-vector-players.sh puts the bodies,
// player_<id>_<variant>.js; MOOMBOX_EJS_VECTOR_PLAYERS names another
// directory. internal/cipher/testdata is gitignored.
var ejsVectorPlayersDir = filepath.Join("testdata", "ejsvectors")

type ejsVectorStep struct {
	In   string `json:"in"`
	Want string `json:"want"`
}

type ejsVectorTest struct {
	Source   string          `json:"source"`
	Player   string          `json:"player"`
	Variants []string        `json:"variants"`
	N        []ejsVectorStep `json:"n"`
	Sig      []ejsVectorStep `json:"sig"`
}

type ejsVectors struct {
	// Variants maps a variant name to its path below /s/player/<id>/, as
	// upstream's tests.ts names them.
	Variants map[string]string `json:"variants"`
	Tests    []ejsVectorTest   `json:"tests"`
}

func readEjsVectors(t *testing.T) ejsVectors {
	t.Helper()
	b, err := os.ReadFile(ejsVectorsFile)
	if err != nil {
		t.Fatalf("read %s: %v", ejsVectorsFile, err)
	}
	var v ejsVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse %s: %v", ejsVectorsFile, err)
	}
	return v
}

// ejsVectorBody returns the body of one variant of a player: from the vector
// players directory, or for the main variant from testdata/player_<id>.js,
// the fixtures the other cipher tests read (cb017549 is fetched by
// fetch-cipher-fixture.sh). ok is false when neither is there.
func ejsVectorBody(dir, player, variant string) (string, bool) {
	paths := []string{filepath.Join(dir, "player_"+player+"_"+variant+".js")}
	if variant == "main" {
		paths = append(paths, filepath.Join("testdata", "player_"+player+".js"))
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// ejsVariantGroup is the row of the tally a variant is counted in.
func ejsVariantGroup(variant string) string {
	switch {
	case strings.HasSuffix(variant, "tcl"):
		return "tcl"
	case strings.HasPrefix(variant, "tv"):
		return "tv"
	case strings.Contains(variant, "es6"):
		return "es6"
	case variant == "main":
		return "main"
	}
	return "other"
}

// TestEjsPortVectors is the gate for the Go port of ejs (preprocessPlayer):
// for every player body present, every expected n and sig answer in
// ejs_vectors.json, the variants upstream marks as failing left out as
// upstream leaves them out. A body that is not there is skipped; with none
// at all the test skips, unless MOOMBOX_REQUIRE_CIPHER_FIXTURE=1 (CI's
// switch, set once fetch-cipher-fixture.sh succeeded), when the cb017549
// fixture must be there.
//
// It logs a tally per variant group and the time and heap each body cost.
// The sidecar's tally on the same vectors is ejs's own; the port must equal
// it, which on the bodies upstream marks as passing means every pair.
func TestEjsPortVectors(t *testing.T) {
	vec := readEjsVectors(t)
	dir := ejsVectorPlayersDir
	if d := os.Getenv("MOOMBOX_EJS_VECTOR_PLAYERS"); d != "" {
		dir = d
	}
	type tally struct{ bodies, passed, failed int }
	tot := map[string]*tally{}
	bodies := 0
	var prepMax, runMax time.Duration
	for _, tc := range vec.Tests {
		for _, variant := range tc.Variants {
			js, ok := ejsVectorBody(dir, tc.Player, variant)
			if !ok {
				continue
			}
			bodies++
			g := ejsVariantGroup(variant)
			if tot[g] == nil {
				tot[g] = &tally{}
			}
			tot[g].bodies++
			name := tc.Player + "_" + variant
			fails := 0
			t0 := time.Now()
			code, st, err := preprocessPlayer(js)
			prep := time.Since(t0)
			if err != nil {
				t.Errorf("%s (%s): preprocessPlayer: %v", name, tc.Source, err)
				tot[g].failed++
				continue
			}
			t0 = time.Now()
			solvers, err := getFromPrepared(code)
			run := time.Since(t0)
			if err != nil {
				t.Errorf("%s (%s): getFromPrepared: %v", name, tc.Source, err)
				tot[g].failed++
				continue
			}
			prepMax, runMax = max(prepMax, prep), max(runMax, run)
			check := func(kind string, steps []ejsVectorStep, solve func(string) (string, error)) {
				for _, s := range steps {
					got, err := solve(s.In)
					if err != nil || got != s.Want {
						fails++
						t.Errorf("%s (%s) %s(%q) = %q, %v; want %q", name, tc.Source, kind, s.In, got, err, s.Want)
					}
				}
			}
			check("n", tc.N, solvers.DecryptN)
			check("sig", tc.Sig, solvers.DecryptSig)
			if fails == 0 {
				tot[g].passed++
			} else {
				tot[g].failed++
			}
			t.Logf("%-20s solvers=%d routes=%s prepare=%s compile=%s", name, st.solvers, st.routes,
				prep.Round(time.Millisecond), run.Round(time.Millisecond))
		}
	}
	if bodies == 0 {
		if os.Getenv("MOOMBOX_REQUIRE_CIPHER_FIXTURE") == "1" {
			t.Fatal("no vector player body found with MOOMBOX_REQUIRE_CIPHER_FIXTURE=1: testdata/player_cb017549.js should be there (bash .github/scripts/fetch-cipher-fixture.sh)")
		}
		t.Skip("no vector player bodies: bash .github/scripts/fetch-cipher-fixture.sh fetches the cb017549 one, fetch-ejs-vector-players.sh the rest")
	}
	groups := make([]string, 0, len(tot))
	for g := range tot {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var line []string
	for _, g := range groups {
		line = append(line, fmt.Sprintf("%s %d/%d", g, tot[g].passed, tot[g].bodies))
	}
	t.Logf("bodies solved, every pair right: %s; slowest prepare %s, slowest compile %s",
		strings.Join(line, ", "), prepMax.Round(time.Millisecond), runMax.Round(time.Millisecond))
}

// TestEjsVectorsAreWellFormed: every test names a player, known variants and
// at least one pair, so a hand edit or a regeneration that drops something
// fails here rather than silently testing less. It also holds the one entry
// CI can always run, cb017549's main variant.
func TestEjsVectorsAreWellFormed(t *testing.T) {
	vec := readEjsVectors(t)
	haveFixture := false
	for i, tc := range vec.Tests {
		if len(tc.Player) < 8 || len(tc.Variants) == 0 || len(tc.N)+len(tc.Sig) == 0 || tc.Source == "" {
			t.Errorf("test %d (%s): player, source, variants or pairs missing", i, tc.Player)
		}
		for _, v := range tc.Variants {
			if vec.Variants[v] == "" {
				t.Errorf("test %d (%s): variant %q has no path", i, tc.Player, v)
			}
		}
		if tc.Player == "cb017549" && len(tc.Sig) > 0 && len(tc.N) > 0 {
			haveFixture = true
		}
	}
	if !haveFixture {
		t.Error("no n and sig pairs for cb017549, the fixture CI fetches")
	}
}

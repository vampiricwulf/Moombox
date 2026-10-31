package cipher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// nTestPlayerURL is the player every row below solves under. Its compiled
// goja solvers are planted straight into the resolver's in-memory cache
// (gojaWithN), so nothing is fetched.
const nTestPlayerURL = "https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js"

// The n challenge every row carries, and the answer a working solver gives.
const (
	nChallenge = "abcdefghijk"
	nAnswer    = "SOLVEDvalue"
)

// gojaWithN is a GojaResolver whose compile of nTestPlayerURL is solvers: n
// the player's n function, or nil for a player the extractor found none in.
func gojaWithN(t *testing.T, n func(string) (string, error)) *GojaResolver {
	t.Helper()
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	g.cacheSolvers(CacheKey(nTestPlayerURL), &Solvers{N: n})
	return g
}

// answers returns an n function that answers every challenge with out(c).
func answers(out func(challenge string) string) func(string) (string, error) {
	return func(c string) (string, error) { return out(c), nil }
}

// TestNUnsolvedFailsTheURL is audit G9 and G14 at the cipher package's three
// entry points. An n challenge that no solver answered used to leave the URL
// as YouTube sent it, err nil, behind a Warn that "downloads may be
// throttled" — but such URLs are refused, which is why yt-dlp skips the
// format (800ec085c, _video.py:3570-3580) — and an answer was installed
// whatever it was, the player's exception sentinel included (an n function
// that throws returns "<prefix>_<challenge>", which yt-dlp's
// validate_nsig_challenge_output rejects: jsc/_director.py:265-278). Each row
// is a way of getting no answer, and each entry point now fails the URL with
// ErrNUnsolved and hands back no URL.
//
// Mutants: checkNResult's suffix arm dropped (the sentinel rows install
// "enhanced_except_…", the identity row the challenge), its empty arm
// dropped (that row installs an empty n); solveN not checking the routed
// answer (every routed row but the error one installs it) or the goja answer
// (the goja rows install it); RoutedResolveURL's failure turned back into a
// Warn and the URL as it came (every RoutedResolveURL row returns the URL);
// RoutedDecryptNInURL's likewise, per form (that form's rows return the
// URL); ResolveURL's missing-n-function case back to a Warn (the "player
// with no n function" row of the goja-only entry resolves).
func TestNUnsolvedFailsTheURL(t *testing.T) {
	sentinel := func(c string) string { return "enhanced_except_AAAAAAAA_" + c }
	for _, tc := range []struct {
		name   string
		routed Solver                       // nil: no routed solver
		gojaN  func(string) (string, error) // the goja compile's n function
		noGoja bool                         // no goja resolver at all
	}{
		{name: "the routed solver fails, no goja", routed: &staticSolver{nErr: errors.New("sidecar down")}, noGoja: true},
		{name: "the routed solver answers with the exception sentinel", routed: &staticSolver{n: map[string]string{nChallenge: sentinel(nChallenge)}}, noGoja: true},
		{name: "the routed solver answers with the challenge itself", routed: &staticSolver{n: map[string]string{nChallenge: nChallenge}}, noGoja: true},
		{name: "the routed solver answers with nothing", routed: &staticSolver{n: map[string]string{nChallenge: ""}}, noGoja: true},
		{name: "routed and goja both answer with the sentinel", routed: &staticSolver{n: map[string]string{nChallenge: sentinel(nChallenge)}}, gojaN: answers(sentinel)},
		{name: "the routed solver fails, goja answers with the sentinel", routed: &staticSolver{nErr: errors.New("sidecar down")}, gojaN: answers(sentinel)},
		{name: "the routed solver fails, goja's player has no n function", routed: &staticSolver{nErr: errors.New("sidecar down")}},
		{name: "goja alone, answering with the sentinel", gojaN: answers(sentinel)},
		{name: "goja alone, its player having no n function"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var goja *GojaResolver
			if !tc.noGoja {
				goja = gojaWithN(t, tc.gojaN)
			}
			for _, entry := range []struct {
				name string
				call func() (string, error)
			}{
				{"ResolveFormatURL (query)", func() (string, error) {
					return ResolveFormatURL(context.Background(), tc.routed, goja, nTestPlayerURL,
						ResolveFormatRequest{URL: "https://r1.googlevideo.com/videoplayback?itag=18&n=" + nChallenge + "&c=TVHTML5"})
				}},
				{"RoutedDecryptNInURL (query)", func() (string, error) {
					return RoutedDecryptNInURL(context.Background(), tc.routed, goja, nTestPlayerURL,
						"https://r1.googlevideo.com/videoplayback?itag=18&n="+nChallenge)
				}},
				{"RoutedDecryptNInURL (path)", func() (string, error) {
					return RoutedDecryptNInURL(context.Background(), tc.routed, goja, nTestPlayerURL,
						"https://r1.googlevideo.com/videoplayback/id/abc.1/itag/299/n/"+nChallenge+"/sq/1")
				}},
			} {
				got, err := entry.call()
				if !errors.Is(err, ErrNUnsolved) {
					t.Errorf("%s = %q, %v; want ErrNUnsolved", entry.name, got, err)
					continue
				}
				if got != "" {
					t.Errorf("%s handed back %q with its error; want no URL", entry.name, got)
				}
				for _, leak := range []string{nChallenge, "enhanced_except"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("%s error quotes the n or its answer (%q): %v", entry.name, leak, err)
					}
				}
			}
		})
	}
}

// TestNUnsolvedWithNothingToSolveWith: an n with no player URL, no solver at
// all, or a player that cannot be had has no answer either.
// RoutedDecryptNInURL asks no solver without a player (there is no player id
// to ask under), and RoutedResolveURL and ResolveURL mark their pre-solve
// failures ErrNUnsolved when an n is all the URL needed a player for — a URL
// that also carries an encrypted signature fails over that, and says so
// alone.
//
// Mutants: RoutedDecryptNInURL's no-player guard dropped (the solver is asked
// under player ""); unsolvedIfNOnly always wrapping (the signature rows
// match ErrNUnsolved) or never (the n-only rows do not); ResolveURL's failed
// GetSolvers returned unmarked (the "player that cannot be fetched, an n"
// row does not match).
func TestNUnsolvedWithNothingToSolveWith(t *testing.T) {
	asked := 0
	routed := &fakeRoutedSolver{NFn: func(string) (string, error) { asked++; return nAnswer, nil }}
	got, err := RoutedDecryptNInURL(context.Background(), routed, nil, "",
		"https://r1.googlevideo.com/videoplayback/id/abc.1/n/"+nChallenge+"/sq/1")
	if !errors.Is(err, ErrNUnsolved) || got != "" {
		t.Errorf("RoutedDecryptNInURL with no player URL = %q, %v; want \"\" and ErrNUnsolved", got, err)
	}
	if asked != 0 {
		t.Errorf("the solver was asked %d times with no player URL; want none", asked)
	}

	goja := gojaWithN(t, answers(func(string) string { return nAnswer }))
	const withN = "https://r1.googlevideo.com/videoplayback?itag=18&n=" + nChallenge
	for _, tc := range []struct {
		name       string
		call       func() error
		wantUnsolv bool
	}{
		{"no solver, an n", func() error {
			_, err := ResolveFormatURL(context.Background(), nil, nil, nTestPlayerURL, ResolveFormatRequest{URL: withN})
			return err
		}, true},
		{"no solver, an n and a signature", func() error {
			_, err := ResolveFormatURL(context.Background(), nil, nil, nTestPlayerURL, ResolveFormatRequest{URL: withN, EncryptedSig: "ENC"})
			return err
		}, false},
		{"no player URL, an n", func() error {
			_, err := goja.ResolveURL(context.Background(), ResolveURLRequest{StreamURL: withN})
			return err
		}, true},
		{"no player URL, an n and a signature", func() error {
			_, err := goja.ResolveURL(context.Background(), ResolveURLRequest{StreamURL: withN, EncryptedSignature: "ENC"})
			return err
		}, false},
		// No routed solver, so RoutedResolveURL hands the request to
		// ResolveURL, whose GetSolvers fails at the player fetch.
		{"a player that cannot be fetched, an n", func() error {
			_, err := ResolveFormatURL(context.Background(), nil, newFailingGojaResolver(t), unreachablePlayerURL, ResolveFormatRequest{URL: withN})
			return err
		}, true},
		{"a player that cannot be fetched, an n and a signature", func() error {
			_, err := ResolveFormatURL(context.Background(), nil, newFailingGojaResolver(t), unreachablePlayerURL, ResolveFormatRequest{URL: withN, EncryptedSig: "ENC"})
			return err
		}, false},
	} {
		err := tc.call()
		if err == nil {
			t.Errorf("%s: resolved; want an error", tc.name)
			continue
		}
		if got := errors.Is(err, ErrNUnsolved); got != tc.wantUnsolv {
			t.Errorf("%s: errors.Is(err, ErrNUnsolved) = %v, want %v: %v", tc.name, got, tc.wantUnsolv, err)
		}
	}
}

// TestNInvalidRoutedAnswerFallsToGoja: a routed answer that is no answer is
// treated as the routed solver failing, and the goja compile is asked next,
// as yt-dlp's director asks its next provider after an invalid response
// (jsc/_director.py:105-124). Both URL forms get the goja answer.
//
// Mutant: solveN not checking the routed answer (the sentinel is installed
// and goja never asked).
func TestNInvalidRoutedAnswerFallsToGoja(t *testing.T) {
	routed := &staticSolver{n: map[string]string{nChallenge: "enhanced_except_AAAAAAAA_" + nChallenge}}
	goja := gojaWithN(t, answers(func(string) string { return nAnswer }))

	res, err := RoutedResolveURL(context.Background(), routed, goja, ResolveURLRequest{
		StreamURL: "https://r1.googlevideo.com/videoplayback?itag=18&n=" + nChallenge + "&c=TVHTML5",
		PlayerURL: nTestPlayerURL,
	})
	if err != nil {
		t.Fatalf("RoutedResolveURL: %v", err)
	}
	if want := "https://r1.googlevideo.com/videoplayback?itag=18&n=" + nAnswer + "&c=TVHTML5"; res.URL != want {
		t.Errorf("RoutedResolveURL = %q, want goja's answer in place: %q", res.URL, want)
	}

	got, err := RoutedDecryptNInURL(context.Background(), routed, goja, nTestPlayerURL,
		"https://r1.googlevideo.com/videoplayback/id/abc.1/n/"+nChallenge+"/sq/1")
	if err != nil {
		t.Fatalf("RoutedDecryptNInURL: %v", err)
	}
	if want := "https://r1.googlevideo.com/videoplayback/id/abc.1/n/" + nAnswer + "/sq/1"; got != want {
		t.Errorf("RoutedDecryptNInURL = %q, want goja's answer in place: %q", got, want)
	}
}

// TestNPathFormIsReplaced: the path form /n/<challenge>/ — what live DASH
// segment URLs and HLS manifest URLs carry — gets its answer in place, the
// rest of the URL untouched, and a URL carrying both forms has each replaced
// with its own answer.
//
// Mutants: the path form skipped (the first row keeps its challenge); its
// answer written into the query form's place (the second row's query keeps
// its challenge).
func TestNPathFormIsReplaced(t *testing.T) {
	routed := &staticSolver{n: map[string]string{nChallenge: nAnswer, "qqqqqqqqqqq": "QANSWER"}}
	for _, tc := range []struct{ in, want string }{
		{
			"https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/n/" + nChallenge + "/sq/1",
			"https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/n/" + nAnswer + "/sq/1",
		},
		{
			"https://manifest.googlevideo.com/api/manifest/hls_variant/n/" + nChallenge + "/file/index.m3u8?n=qqqqqqqqqqq&x=1",
			"https://manifest.googlevideo.com/api/manifest/hls_variant/n/" + nAnswer + "/file/index.m3u8?n=QANSWER&x=1",
		},
	} {
		got, err := RoutedDecryptNInURL(context.Background(), routed, nil, nTestPlayerURL, tc.in)
		if err != nil {
			t.Errorf("RoutedDecryptNInURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RoutedDecryptNInURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// TestNAnswerThatDoesNotLandIsUnsolved: an n handed in (NParam) for a URL
// whose query carries none has an answer with nowhere to go, and a URL
// resolved without its answer in place has solved nothing — yt-dlp's
// `solved_n = n_result in manifest_path` (_video.py:3699, 3737). It used to
// resolve to the URL as it was. Both resolvers check it.
//
// Mutant: replaceQueryN's miss ignored in RoutedResolveURL (the routed row
// resolves) or in ResolveURL (the goja row resolves).
func TestNAnswerThatDoesNotLandIsUnsolved(t *testing.T) {
	const noN = "https://r1.googlevideo.com/videoplayback?itag=18&c=TVHTML5"
	routed := &staticSolver{n: map[string]string{nChallenge: nAnswer}}
	goja := gojaWithN(t, answers(func(string) string { return nAnswer }))
	req := ResolveURLRequest{StreamURL: noN, PlayerURL: nTestPlayerURL, NParam: nChallenge}

	if res, err := RoutedResolveURL(context.Background(), routed, goja, req); !errors.Is(err, ErrNUnsolved) {
		t.Errorf("RoutedResolveURL = %+v, %v; want ErrNUnsolved", res, err)
	}
	if res, err := goja.ResolveURL(context.Background(), req); !errors.Is(err, ErrNUnsolved) {
		t.Errorf("GojaResolver.ResolveURL = %+v, %v; want ErrNUnsolved", res, err)
	}
}

// TestNThatDoesNotDecodeIsUnsolved: a query n whose raw value does not
// percent-decode is an n present with no challenge to hand a solver
// (RawQueryParam reads it as ""). All three entry points used to skip it and
// hand the URL back as it came, err nil — the n left as YouTube sent it, as
// refused as one no solver answered. Now each fails it with ErrNUnsolved and
// no URL, without asking a solver and without quoting the value. The solver
// here would answer anything, so a row that asks it resolves.
//
// Mutants: the undecodable check dropped in RoutedResolveURL (the
// ResolveFormatURL row resolves), in ResolveURL (the goja row), or in
// RoutedDecryptNInURL (its row).
func TestNThatDoesNotDecodeIsUnsolved(t *testing.T) {
	const raw = "ab%zzcdefghij"
	const withN = "https://r1.googlevideo.com/videoplayback?itag=18&n=" + raw + "&c=TVHTML5"
	routed := &fakeRoutedSolver{NFn: func(string) (string, error) { return nAnswer, nil }}
	goja := gojaWithN(t, answers(func(string) string { return nAnswer }))
	for _, entry := range []struct {
		name string
		call func() (string, error)
	}{
		{"ResolveFormatURL", func() (string, error) {
			return ResolveFormatURL(context.Background(), routed, goja, nTestPlayerURL, ResolveFormatRequest{URL: withN})
		}},
		{"GojaResolver.ResolveURL", func() (string, error) {
			res, err := goja.ResolveURL(context.Background(), ResolveURLRequest{StreamURL: withN, PlayerURL: nTestPlayerURL})
			if res != nil {
				return res.URL, err
			}
			return "", err
		}},
		{"RoutedDecryptNInURL", func() (string, error) {
			return RoutedDecryptNInURL(context.Background(), routed, goja, nTestPlayerURL, withN)
		}},
	} {
		got, err := entry.call()
		if !errors.Is(err, ErrNUnsolved) || got != "" {
			t.Errorf("%s = %q, %v; want \"\" and ErrNUnsolved", entry.name, got, err)
			continue
		}
		if strings.Contains(err.Error(), raw) {
			t.Errorf("%s error quotes the n: %v", entry.name, err)
		}
	}
}

// TestCheckNResult pins what counts as an answer, row by row.
func TestCheckNResult(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool // an answer
	}{
		{nAnswer, true},
		{"x" + nChallenge + "y", true}, // contains the challenge, does not end with it
		{"", false},
		{nChallenge, false},
		{"enhanced_except_AAAAAAAA_" + nChallenge, false},
		{"_" + nChallenge, false},
	} {
		if got := checkNResult(nChallenge, tc.out) == nil; got != tc.want {
			t.Errorf("checkNResult(%q, %q) answer = %v, want %v", nChallenge, tc.out, got, tc.want)
		}
	}
}

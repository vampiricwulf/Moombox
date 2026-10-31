package youtube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDeduplicateFormatsKeepsATokenCarryingShadowOfATokenFreeWinner pins the
// token-carrying shadow (Format.TokenAlternate), TokenFreeAlternate's mirror:
// behind a winner whose client needs no GVS token — tv or web_embedded, the
// usual winner — dedup keeps the lowest-AuthLevel copy of the same stream
// from a client that DOES need one, for the VOD refresh's token fallback to
// move a refused token-free stream onto. The winner is the one it always
// was; a winner carries one shadow or the other, never both; and the copy
// keeps its own response's AvailableAt, since the fallback waits out that
// response's ad.
//
// Mutants this kills:
//   - no TokenAlternate kept → the tv_auth and web_embedded rows
//   - the first-listed copy kept instead of the lowest level → the
//     web_safari-first row (keeps web_safari, not the watch page)
//   - a TokenAlternate kept on a token-requiring winner too → the web-family
//     row (the winner shadows itself)
//   - the shadow choosing the winner → every row's winner check
//   - the copy's AvailableAt taken from the winner → the availability check
func TestDeduplicateFormatsKeepsATokenCarryingShadowOfATokenFreeWinner(t *testing.T) {
	lvl := map[string]int{
		"tv_public":    AuthLevelTVPublic,
		"web_embedded": AuthLevelWebEmbedded,
		"tv_auth":      AuthLevelTVAuth,
		"watch_page":   AuthLevelWatchPageAuth,
		"web_safari":   AuthLevelWebSafari,
		"web":          AuthLevelWeb,
		"web_creator":  AuthLevelWebCreator,
		"visionos":     AuthLevelVisionOS,
	}
	base := time.Unix(1_800_000_000, 0)
	// Each client's response has an ad of its own, a second per level.
	availableAt := func(source string) time.Time { return base.Add(time.Duration(lvl[source]+1) * time.Second) }
	mk := func(source string) Format {
		l := lvl[source]
		return Format{Itag: 137, URL: "https://" + source + "/v137", MimeType: `video/mp4; codecs="avc1.640028"`,
			ContentLength: "1000", Source: source, AuthLevel: &l, AvailableAt: availableAt(source)}
	}
	for _, tc := range []struct {
		name          string
		sources       []string
		wantWinner    string
		wantTokenAlt  string // "" = none
		wantTokenFree string // "" = none
	}{
		{"a tv_auth winner keeps the watch page's copy", []string{"tv_auth", "watch_page"}, "tv_auth", "watch_page", ""},
		{"the lowest-level copy of the class, wherever it is listed", []string{"web_safari", "tv_auth", "web", "watch_page"}, "tv_auth", "watch_page", ""},
		{"a web_embedded winner keeps web_creator's copy", []string{"web_creator", "web_embedded", "visionos"}, "web_embedded", "web_creator", ""},
		{"a tv_public winner with only a token-free loser has none", []string{"visionos", "tv_public"}, "tv_public", "", ""},
		{"a lone tv_auth has none", []string{"tv_auth"}, "tv_auth", "", ""},
		{"a web-family winner carries the token-free shadow only", []string{"web_safari", "watch_page", "visionos"}, "watch_page", "", "visionos"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := make([]Format, 0, len(tc.sources))
			for _, s := range tc.sources {
				pool = append(pool, mk(s))
			}
			got := deduplicateFormats(context.Background(), pool)
			if len(got) != 1 {
				t.Fatalf("dedup returned %d rows, want 1: %+v", len(got), got)
			}
			w := got[0]
			if w.Source != tc.wantWinner || w.URL != "https://"+tc.wantWinner+"/v137" {
				t.Errorf("winner = %s %q, want %s — a shadow must never change which copy wins", w.Source, w.URL, tc.wantWinner)
			}
			for _, shadow := range []struct {
				name string
				got  *Format
				want string
			}{
				{"TokenAlternate", w.TokenAlternate, tc.wantTokenAlt},
				{"TokenFreeAlternate", w.TokenFreeAlternate, tc.wantTokenFree},
			} {
				if shadow.want == "" {
					if shadow.got != nil {
						t.Errorf("%s = %s %q, want none", shadow.name, shadow.got.Source, shadow.got.URL)
					}
					continue
				}
				alt := shadow.got
				if alt == nil {
					t.Errorf("%s = nil, want the %s copy", shadow.name, shadow.want)
					continue
				}
				if alt.Source != shadow.want || alt.URL != "https://"+shadow.want+"/v137" || alt.Itag != 137 || alt.ContentLength != "1000" {
					t.Errorf("%s = %s %q itag %d clen %s, want the %s copy of the same file", shadow.name, alt.Source, alt.URL, alt.Itag, alt.ContentLength, shadow.want)
				}
				if !alt.AvailableAt.Equal(availableAt(shadow.want)) {
					t.Errorf("%s available at %v, want %v — its own response's", shadow.name, alt.AvailableAt, availableAt(shadow.want))
				}
				if alt.TokenAlternate != nil || alt.TokenFreeAlternate != nil {
					t.Errorf("%s carries a shadow of its own: %+v / %+v", shadow.name, alt.TokenAlternate, alt.TokenFreeAlternate)
				}
			}
		})
	}
}

// TestDeduplicateFormatsDropsAStaleTokenCarryingShadow: dedup recomputes
// every shadow from the pool it is handed, the token-carrying kind included —
// a stale one on the winner is replaced, and a stale one on the copy that
// becomes the shadow does not ride along into a chain.
//
// Mutants this kills:
//   - not clearing the winner's incoming TokenAlternate → the lone row keeps it
//   - not clearing the alternate's incoming shadows → the chain row
func TestDeduplicateFormatsDropsAStaleTokenCarryingShadow(t *testing.T) {
	tv, wp, vo := AuthLevelTVAuth, AuthLevelWatchPageAuth, AuthLevelVisionOS
	stale := &Format{Itag: 137, URL: "https://stale/v137", Source: "web_safari"}
	staleFree := &Format{Itag: 137, URL: "https://stale-free/v137", Source: "visionos", AuthLevel: &vo}

	t.Run("lone", func(t *testing.T) {
		pool := []Format{{Itag: 137, URL: "https://tv_auth/v137", Source: "tv_auth", AuthLevel: &tv, TokenAlternate: stale}}
		if got := deduplicateFormats(context.Background(), pool); len(got) != 1 || got[0].TokenAlternate != nil {
			t.Errorf("a lone token-free winner kept a stale token-carrying shadow: %+v", got)
		}
	})

	t.Run("chain", func(t *testing.T) {
		pool := []Format{
			{Itag: 137, URL: "https://tv_auth/v137", Source: "tv_auth", AuthLevel: &tv, TokenAlternate: stale},
			{Itag: 137, URL: "https://watch_page/v137", Source: "watch_page", AuthLevel: &wp, TokenFreeAlternate: staleFree, TokenAlternate: stale},
		}
		got := deduplicateFormats(context.Background(), pool)
		if len(got) != 1 || got[0].TokenAlternate == nil {
			t.Fatalf("want the tv_auth winner with a watch_page shadow, got %+v", got)
		}
		if alt := got[0].TokenAlternate; alt.URL != "https://watch_page/v137" || alt.TokenFreeAlternate != nil || alt.TokenAlternate != nil {
			t.Errorf("shadow = %q carrying %+v / %+v, want https://watch_page/v137 with no shadow of its own", alt.URL, alt.TokenFreeAlternate, alt.TokenAlternate)
		}
		if pool[0].TokenAlternate != stale || pool[1].TokenFreeAlternate != staleFree {
			t.Errorf("dedup rewrote its input's shadow pointers")
		}
	})
}

// TestMergeFormatPoolKeepsTheTokenCarryingShadow: the missing_pot re-extract
// merges the cookieless chain into a copy of an already-deduplicated pool, and
// a token-carrying shadow the pool carried must survive it — dedup recomputes
// every shadow from the slice it is handed, so the merge feeds it back in as a
// candidate, as it does the token-free kind.
//
// Mutant: MergeFormatPool re-including TokenFreeAlternate alone — the tv_auth
// winner comes out with no TokenAlternate.
func TestMergeFormatPoolKeepsTheTokenCarryingShadow(t *testing.T) {
	tv, wp, vo := AuthLevelTVAuth, AuthLevelWatchPageAuth, AuthLevelVisionOS
	shadow := &Format{Itag: 137, URL: "https://watch_page/v137", Source: "watch_page", AuthLevel: &wp}
	pool := []Format{{Itag: 137, URL: "https://tv_auth/v137", Source: "tv_auth", AuthLevel: &tv, TokenAlternate: shadow}}
	extra := []Format{{Itag: 137, URL: "https://visionos/v137", Source: "visionos", AuthLevel: &vo}}

	got := MergeFormatPool(context.Background(), pool, extra)
	if len(got) != 1 || got[0].Source != "tv_auth" {
		t.Fatalf("merged pool = %+v, want the tv_auth winner alone", got)
	}
	if alt := got[0].TokenAlternate; alt == nil || alt.URL != shadow.URL || alt.Source != "watch_page" {
		t.Errorf("TokenAlternate after the merge = %+v, want the watch_page copy the pool carried", alt)
	}
	if got[0].TokenFreeAlternate != nil {
		t.Errorf("a token-free winner carries a token-free shadow after the merge: %+v", got[0].TokenFreeAlternate)
	}
	if pool[0].TokenAlternate != shadow {
		t.Errorf("MergeFormatPool rewrote its input's shadow pointer")
	}
}

// TestFormatJSONOmitsTheTokenAlternate: the token-carrying shadow is never
// serialised either — a resume re-extracts, and a persisted URL would be
// stale anyway.
//
// Mutant: TokenAlternate's json:"-" tag dropped.
func TestFormatJSONOmitsTheTokenAlternate(t *testing.T) {
	alt := Format{Itag: 137, URL: "https://watch_page/v137", Source: "watch_page"}
	f := Format{Itag: 137, URL: "https://tv_auth/v137", Source: "tv_auth", TokenAlternate: &alt}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if s := string(b); strings.Contains(s, "watch_page") || strings.Contains(strings.ToLower(s), "alternate") {
		t.Errorf("marshalled Format carries the shadow: %s", s)
	}
}

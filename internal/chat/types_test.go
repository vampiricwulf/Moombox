package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// preArcArchiveLine is one message exactly as v2.8.8 wrote it: a paid
// membership milestone, with `superchat` and `isMembership` but no
// `membershipText` key at all — that field did not exist yet (2026-09-25,
// Arc K). Every archive on disk before the upgrade is made of lines like it.
const preArcArchiveLine = `{"id":"m1","timestampUsec":"1700000000000000",` +
	`"timestampText":"1:23","offsetMs":83000,"hasOffset":true,` +
	`"authorName":"a member","authorChannelId":"UCx","authorBadges":["member"],` +
	`"message":[{"type":"text","text":"six months!"}],` +
	`"superchat":{"amount":"$2.00","currency":"USD","color":"cyan","tier":2,` +
	`"kind":"message","headerColor":"#00B8D4","bodyColor":"#00E5FF"},` +
	`"isMembership":true}`

// TestPreArcArchiveLineReadsWithAnEmptyMembershipText pins the compatibility
// half of MembershipText's contract (internal/chat/types.go): an archive
// written before the field existed must read back exactly as it did, with the
// new field at its zero value, and a message that has no membership line must
// not grow a key on the way out.
//
// THE MUTANT: drop `,omitempty` from the `membershipText` tag, or rename the
// key, and nothing else in the suite fails — the parse-side tests only ever
// build messages in memory, and the player reads `msg.membershipText` by name.
func TestPreArcArchiveLineReadsWithAnEmptyMembershipText(t *testing.T) {
	var old ChatMessage
	if err := json.Unmarshal([]byte(preArcArchiveLine), &old); err != nil {
		t.Fatalf("unmarshal the v2.8.8 line: %v", err)
	}
	if old.MembershipText != "" {
		t.Errorf("MembershipText = %q, want %q — a pre-arc archive has no line to carry",
			old.MembershipText, "")
	}

	// The same line with the field present but empty — what HEAD would write
	// for a message with no membership header. The two must be indistinguishable
	// after a round trip, field for field, not only in MembershipText.
	withKey := strings.Replace(preArcArchiveLine, `"isMembership":true}`,
		`"isMembership":true,"membershipText":""}`, 1)
	if withKey == preArcArchiveLine {
		t.Fatal("the fixture no longer ends in isMembership; fix the splice above")
	}
	var current ChatMessage
	if err := json.Unmarshal([]byte(withKey), &current); err != nil {
		t.Fatalf("unmarshal the present-but-empty line: %v", err)
	}
	if !reflect.DeepEqual(old, current) {
		t.Errorf("an absent membershipText and an empty one deserialize differently:\n old %+v\nnew %+v",
			old, current)
	}

	// And the marshal half: an empty line writes no key, so HEAD's archives
	// stay byte-comparable with the old ones for every non-membership message.
	out, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "membershipText") {
		t.Errorf("an empty MembershipText was written out: %s", out)
	}

	// The positive control: a real line does survive the same round trip, so
	// the assertion above is about omitempty and not about a dropped field.
	current.MembershipText = "Member for 6 months"
	spoken, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal with a line: %v", err)
	}
	if !strings.Contains(string(spoken), `"membershipText":"Member for 6 months"`) {
		t.Errorf("a real membership line did not reach the archive: %s", spoken)
	}
}

// playerJSPath is the browser half of the Super Chat palette. The seven tier
// pairs there are the player's fallback for an archive written before
// headerColor/bodyColor were recorded, so they must stay identical to the
// pairs internal/chat resolves tiers from — a change to one table and not the
// other repaints old cards in the wrong tier's colours with nothing failing.
const playerJSPath = "../../web/public/modules/player.js"

var (
	jsTierBlockRe = regexp.MustCompile(`(?s)export const SUPERCHAT_TIER_COLORS = \{(.*?)\n\};`)
	jsTierRowRe   = regexp.MustCompile(
		`(?m)^\s*(\d+):\s*\{\s*header:\s*"(#[0-9A-Fa-f]{6})",\s*body:\s*"(#[0-9A-Fa-f]{6})"\s*\},`)
)

// jsTierPair is one row of the player's SUPERCHAT_TIER_COLORS literal.
type jsTierPair struct{ header, body string }

// readPlayerTierPalette parses SUPERCHAT_TIER_COLORS out of player.js as
// source text. Reading the literal rather than running the module keeps this
// pin free of a JS runtime, the same way cmd/moombox's call-site pins read
// their own source; the cost is that the literal's shape is part of the
// contract, which is why a missing block is a Fatal and not a skip.
func readPlayerTierPalette(t *testing.T) map[int]jsTierPair {
	t.Helper()
	src, err := os.ReadFile(playerJSPath)
	if err != nil {
		t.Fatalf("read %s: %v — the player's fallback palette is half of this pin", playerJSPath, err)
	}
	block := jsTierBlockRe.FindSubmatch(src)
	if block == nil {
		t.Fatalf("no `export const SUPERCHAT_TIER_COLORS = {…};` literal in %s — if the table moved, "+
			"move this pin with it rather than deleting it", playerJSPath)
	}
	pairs := map[int]jsTierPair{}
	for _, m := range jsTierRowRe.FindAllStringSubmatch(string(block[1]), -1) {
		tier, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("tier key %q is not a number", m[1])
		}
		pairs[tier] = jsTierPair{header: strings.ToUpper(m[2]), body: strings.ToUpper(m[3])}
	}
	return pairs
}

// TestPlayerTierPaletteMatchesTheGoPalette compares the player's seven tier
// pairs, tier by tier, with superchatTierColors (internal/chat/types.go).
//
// The Go table is keyed by ARGB and its value names only the tier, so which of
// a tier's two colours is the header is not data there — it lives in the
// comments and in TestSuperchatRecordJSONShape's fixture. The comparison is
// therefore per-tier and unordered, which still catches every hex that drifts
// on one side; the header/body roles are pinned separately on each side
// (player.test.mjs's per-tier ink assertions, and the Go fixture above).
//
// THE MUTANT: correct a hex in one language — YouTube repaints tier 5, say —
// and every test in both suites stays green while archives written before the
// colours were recorded render in the old colour on one side only.
func TestPlayerTierPaletteMatchesTheGoPalette(t *testing.T) {
	js := readPlayerTierPalette(t)

	// The Go side: every ARGB key reduced to #RRGGBB, grouped by tier.
	goPairs := map[int][]string{}
	for argb, row := range superchatTierColors {
		goPairs[row.tier] = append(goPairs[row.tier], fmt.Sprintf("#%06X", argb&0xFFFFFF))
	}

	for tier := 1; tier <= 7; tier++ {
		pair, ok := js[tier]
		if !ok {
			t.Errorf("tier %d: player.js has no row; internal/chat resolves %v",
				tier, sorted(goPairs[tier]))
			continue
		}
		if pair.header == pair.body {
			t.Errorf("tier %d: player.js paints header and body the same colour %s",
				tier, pair.header)
		}
		got, want := sorted([]string{pair.header, pair.body}), sorted(goPairs[tier])
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tier %d: player.js has header %s / body %s; internal/chat's table has %s",
				tier, pair.header, pair.body, strings.Join(want, " and "))
		}
	}

	// Tier 0 is the player's neutral "unresolved" pair and has deliberately no
	// row on the Go side — gray is never a resolved tier (owner ruling
	// 2026-09-05). A Go row using either hex would make tier 0 ambiguous.
	zero, ok := js[0]
	if !ok {
		t.Fatalf("player.js has no tier 0 row; it is the fallback for an archive whose colours " +
			"internal/chat could not resolve")
	}
	for _, hex := range []string{zero.header, zero.body} {
		for argb, row := range superchatTierColors {
			if fmt.Sprintf("#%06X", argb&0xFFFFFF) == hex {
				t.Errorf("tier 0's %s is also tier %d's colour in internal/chat — the unresolved "+
					"pair must match no resolved tier", hex, row.tier)
			}
		}
	}

	if len(js) != 8 {
		t.Errorf("player.js has %d tier rows, want 8 (0 plus 1..7): %v", len(js), js)
	}
}

// sorted returns a sorted copy, so a failure prints the two tables in the same
// order however the Go map ranged.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

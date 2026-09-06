package chat

import (
	"encoding/json"
	"sync"
	"testing"
)

// superchatWarnRecorder captures Warn lines WITH their key/value fields, which
// recordingChatLogger drops. Test-only.
type superchatWarnRecorder struct {
	mu     sync.Mutex
	warns  []map[string]any
	debugs []string
}

func (r *superchatWarnRecorder) Debug(msg string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debugs = append(r.debugs, msg)
}

func (r *superchatWarnRecorder) Warn(msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fields := map[string]any{"msg": msg}
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			fields[k] = args[i+1]
		}
	}
	r.warns = append(r.warns, fields)
}

func (r *superchatWarnRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.warns)
}

func newSuperchatTestAPI(rec *superchatWarnRecorder) *ChatAPI {
	api := NewChatAPI("key", "visitor", nil)
	if rec != nil {
		api.Logger = rec
	}
	return api
}

// paidMessage builds the slice of a liveChatPaidMessageRenderer that
// parseSuperChatInfo reads. Colors are YouTube's ARGB ints, delivered by the
// JSON decoder as float64 — exactly as production sees them.
func paidMessage(amount string, header, body any) map[string]any {
	r := map[string]any{
		"purchaseAmountText": map[string]any{"simpleText": amount},
	}
	if header != nil {
		r["headerBackgroundColor"] = header
	}
	if body != nil {
		r["bodyBackgroundColor"] = body
	}
	return r
}

// TestSuperchatTierResolvesFromHeaderBackgroundColor: the parser reads
// headerBackgroundColor, so the tier table MUST be keyed on YouTube's header
// palette. 4278237396 is the value the 2026-09-05 log flagged as unknown (the
// light-blue tier's header); 4294947584 is the header of a real €10 sample.
func TestSuperchatTierResolvesFromHeaderBackgroundColor(t *testing.T) {
	cases := []struct {
		header float64
		tier   int
		color  string
		hex    string
	}{
		{4279592384, 1, "blue", "#1565C0"},
		{4278237396, 2, "cyan", "#00B8D4"},
		{4278239141, 3, "green", "#00BFA5"},
		{4294947584, 4, "yellow", "#FFB300"},
		{4293284096, 5, "orange", "#E65100"},
		{4290910299, 6, "magenta", "#C2185B"},
		{4291821568, 7, "red", "#D00000"},
	}
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)
	for _, tc := range cases {
		got := api.parseSuperChatInfo(paidMessage("$1.00", tc.header, nil))
		if got.Tier != tc.tier || got.Color != tc.color {
			t.Errorf("header %v: got tier %d %q, want %d %q", tc.header, got.Tier, got.Color, tc.tier, tc.color)
		}
		if got.HeaderColor != tc.hex {
			t.Errorf("header %v: HeaderColor = %q, want %q", tc.header, got.HeaderColor, tc.hex)
		}
		if got.Kind != "message" {
			t.Errorf("header %v: Kind = %q, want %q", tc.header, got.Kind, "message")
		}
	}
	if rec.count() != 0 {
		t.Errorf("known header colors must not warn; got %d warnings", rec.count())
	}
}

// TestSuperchatTierFallsBackToBodyBackgroundColor: when the header is missing
// or unrecognised, the body palette (the OLD table's keys) still resolves the
// tier, so a renderer that only carries bodyBackgroundColor is not "unknown".
func TestSuperchatTierFallsBackToBodyBackgroundColor(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	got := api.parseSuperChatInfo(paidMessage("€10.00", nil, float64(4294953512)))
	if got.Tier != 4 || got.Color != "yellow" {
		t.Fatalf("body-only: got tier %d %q, want 4 yellow", got.Tier, got.Color)
	}
	if got.BodyColor != "#FFCA28" || got.HeaderColor != "" {
		t.Errorf("body-only: colors = header %q body %q, want header \"\" body #FFCA28", got.HeaderColor, got.BodyColor)
	}
	if rec.count() != 0 {
		t.Errorf("body-resolved tier must not warn; got %d", rec.count())
	}
}

// TestSuperchatUnknownColorIsTierZeroGrayWithDetails: an unmapped
// color is recorded as tier 0 with the color name "gray" (never silently tier
// 1 blue),
// the raw colors travel with the record as hex, and ONE warning per distinct
// color pair carries everything needed to extend the table later.
func TestSuperchatUnknownColorIsTierZeroGrayWithDetails(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	got := api.parseSuperChatInfo(paidMessage("$3.00", float64(4278190080), float64(4278190081)))
	if got.Tier != 0 || got.Color != "gray" {
		t.Fatalf("unknown: got tier %d color %q, want 0 / gray", got.Tier, got.Color)
	}
	if got.HeaderColor != "#000000" || got.BodyColor != "#000001" {
		t.Errorf("unknown: raw colors = %q / %q, want #000000 / #000001", got.HeaderColor, got.BodyColor)
	}
	if got.Amount != "$3.00" || got.Currency != "USD" {
		t.Errorf("unknown: amount/currency = %q/%q, want $3.00/USD", got.Amount, got.Currency)
	}

	if rec.count() != 1 {
		t.Fatalf("want exactly one warning, got %d", rec.count())
	}
	w := rec.warns[0]
	if w["msg"] != "chat: unknown superchat tier color" {
		t.Errorf("warn msg = %q", w["msg"])
	}
	for k, want := range map[string]any{
		"kind":        "message",
		"headerColor": "#000000",
		"bodyColor":   "#000001",
		"amount":      "$3.00",
	} {
		if w[k] != want {
			t.Errorf("warn field %s = %v, want %v", k, w[k], want)
		}
	}

	// Same unknown pair again: no second warning (one per distinct pair).
	api.parseSuperChatInfo(paidMessage("$3.00", float64(4278190080), float64(4278190081)))
	if rec.count() != 1 {
		t.Errorf("repeat of the same unknown pair must not warn again; got %d", rec.count())
	}
	// A different unknown pair IS a new signal.
	api.parseSuperChatInfo(paidMessage("$3.00", float64(4278190082), nil))
	if rec.count() != 2 {
		t.Errorf("a new unknown pair must warn; got %d", rec.count())
	}
}

// TestSuperStickerTierResolvesFromMoneyChipAndBackground: a Super Sticker has
// no headerBackgroundColor. Its moneyChipBackgroundColor uses the header
// palette and its backgroundColor the body palette; either resolves the tier.
// Before this change every sticker was archived as tier 0 with an empty color.
func TestSuperStickerTierResolvesFromMoneyChipAndBackground(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	sticker := map[string]any{
		"purchaseAmountText":       map[string]any{"simpleText": "$2.00"},
		"moneyChipBackgroundColor": float64(4278237396),
		"backgroundColor":          float64(4278248959),
	}
	got := api.parseSuperStickerInfo(sticker)
	if got.Tier != 2 || got.Color != "cyan" {
		t.Fatalf("sticker: got tier %d %q, want 2 cyan", got.Tier, got.Color)
	}
	if got.Kind != "sticker" {
		t.Errorf("sticker: Kind = %q, want sticker", got.Kind)
	}
	if got.HeaderColor != "#00B8D4" || got.BodyColor != "#00E5FF" {
		t.Errorf("sticker: raw colors = %q / %q, want #00B8D4 / #00E5FF", got.HeaderColor, got.BodyColor)
	}
	if rec.count() != 0 {
		t.Errorf("resolved sticker must not warn; got %d", rec.count())
	}

	// Background alone (chip missing) still resolves through the body palette.
	got = api.parseSuperStickerInfo(map[string]any{
		"purchaseAmountText": map[string]any{"simpleText": "$5.00"},
		"backgroundColor":    float64(4280150454),
	})
	if got.Tier != 3 || got.Color != "green" {
		t.Errorf("sticker background-only: got tier %d %q, want 3 green", got.Tier, got.Color)
	}
}

// TestSuperStickerWithoutColorsIsTierZeroGray: a sticker carrying no color at
// all is tier 0 with color "gray" and warned about — not a silent tier 0.
func TestSuperStickerWithoutColorsIsTierZeroGray(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	got := api.parseSuperStickerInfo(map[string]any{
		"purchaseAmountText": map[string]any{"simpleText": "¥500"},
	})
	if got.Tier != 0 || got.Color != "gray" {
		t.Fatalf("colorless sticker: got tier %d color %q, want 0 / gray", got.Tier, got.Color)
	}
	if rec.count() != 1 || rec.warns[0]["kind"] != "sticker" {
		t.Errorf("colorless sticker must warn once with kind=sticker; warns=%v", rec.warns)
	}
}

// TestParseMessageRoutesStickersToTheStickerParser pins the wiring: the
// liveChatPaidStickerRenderer branch must feed the sticker parser, so archived
// stickers carry Kind "sticker" and a resolved tier.
func TestParseMessageRoutesStickersToTheStickerParser(t *testing.T) {
	api := newSuperchatTestAPI(&superchatWarnRecorder{})
	item := map[string]any{
		"liveChatPaidStickerRenderer": map[string]any{
			"id":                       "sticker-1",
			"timestampUsec":            "1700000000000000",
			"authorName":               map[string]any{"simpleText": "fan"},
			"purchaseAmountText":       map[string]any{"simpleText": "$2.00"},
			"moneyChipBackgroundColor": float64(4278237396),
			"backgroundColor":          float64(4278248959),
		},
	}
	msg := api.parseAction(map[string]any{
		"addChatItemAction": map[string]any{"item": item},
	})
	if msg == nil || msg.Superchat == nil {
		t.Fatal("sticker item produced no superchat record")
	}
	if msg.Superchat.Kind != "sticker" || msg.Superchat.Tier != 2 {
		t.Errorf("sticker record = kind %q tier %d, want sticker / 2", msg.Superchat.Kind, msg.Superchat.Tier)
	}
}

// TestParseActionRoutesPaidMessagesToTheMessageParser is the sibling pin for
// the branch the bug lived in: a liveChatPaidMessageRenderer item must reach
// the message parser and resolve its tier from the header.
func TestParseActionRoutesPaidMessagesToTheMessageParser(t *testing.T) {
	api := newSuperchatTestAPI(&superchatWarnRecorder{})
	msg := api.parseAction(map[string]any{
		"addChatItemAction": map[string]any{"item": map[string]any{
			"liveChatPaidMessageRenderer": map[string]any{
				"id":                    "paid-1",
				"timestampUsec":         "1700000000000000",
				"authorName":            map[string]any{"simpleText": "fan"},
				"purchaseAmountText":    map[string]any{"simpleText": "$2.00"},
				"headerBackgroundColor": float64(4278237396),
				"bodyBackgroundColor":   float64(4278248959),
			},
		}},
	})
	if msg == nil || msg.Superchat == nil {
		t.Fatal("paid message item produced no superchat record")
	}
	if msg.Superchat.Kind != "message" || msg.Superchat.Tier != 2 || msg.Superchat.Color != "cyan" {
		t.Errorf("paid record = kind %q tier %d %q, want message / 2 cyan", msg.Superchat.Kind, msg.Superchat.Tier, msg.Superchat.Color)
	}
}

// TestSuperchatUnmappedHeaderWithMappedBodyResolvesFromTheBody: a header the
// palettes do not know must not hide a body they do — the body resolves the
// tier, the odd header is still recorded, and nothing is warned.
func TestSuperchatUnmappedHeaderWithMappedBodyResolvesFromTheBody(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)
	got := api.parseSuperChatInfo(paidMessage("$10.00", float64(4278190080), float64(4294953512)))
	if got.Tier != 4 || got.Color != "yellow" {
		t.Fatalf("got tier %d %q, want 4 yellow", got.Tier, got.Color)
	}
	if got.HeaderColor != "#000000" {
		t.Errorf("odd header must still be recorded; got %q", got.HeaderColor)
	}
	if rec.count() != 0 {
		t.Errorf("a resolved tier must not warn; got %d", rec.count())
	}
}

// TestSuperchatTableIsCheckedOnBothFields: the table holds header and body
// rows and each field is looked up against all of it. If YouTube (or the
// sticker mapping we inferred) ever puts a header color in the body field or
// vice versa, the tier still resolves instead of becoming unknown.
func TestSuperchatTableIsCheckedOnBothFields(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	// Swapped: body blue in the header field, header blue in the body field.
	got := api.parseSuperChatInfo(paidMessage("$1.00", float64(4280191205), float64(4279592384)))
	if got.Tier != 1 || got.Color != "blue" {
		t.Errorf("swapped fields: got tier %d %q, want 1 blue", got.Tier, got.Color)
	}
	// Sticker with only a header color in backgroundColor.
	got = api.parseSuperStickerInfo(map[string]any{
		"purchaseAmountText": map[string]any{"simpleText": "$5.00"},
		"backgroundColor":    float64(4278239141),
	})
	if got.Tier != 3 || got.Color != "green" {
		t.Errorf("sticker header-color background: got tier %d %q, want 3 green", got.Tier, got.Color)
	}
	if rec.count() != 0 {
		t.Errorf("resolved tiers must not warn; got %d", rec.count())
	}
}

// TestSuperchatUnknownWarnCarriesRawARGBAndDistinguishesAlpha: the hex drops
// the alpha byte, so two unknown values that differ only in alpha look alike
// in HeaderColor. The warning must carry the raw ARGB decimals and the dedup
// must treat them as distinct signals.
func TestSuperchatUnknownWarnCarriesRawARGBAndDistinguishesAlpha(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	got := api.parseSuperChatInfo(paidMessage("$2.00", float64(2147530964), nil)) // 0x8000B8D4
	if got.Tier != 0 || got.Color != "gray" {
		t.Fatalf("alpha-80 light blue must be unknown (palette keys are opaque); got tier %d color %q", got.Tier, got.Color)
	}
	if got.HeaderColor != "#00B8D4" {
		t.Errorf("hex drops alpha: got %q, want #00B8D4", got.HeaderColor)
	}
	if rec.count() != 1 {
		t.Fatalf("want one warning, got %d", rec.count())
	}
	if rec.warns[0]["headerARGB"] != uint32(2147530964) {
		t.Errorf("warn headerARGB = %v (%T), want uint32 2147530964", rec.warns[0]["headerARGB"], rec.warns[0]["headerARGB"])
	}
	if _, ok := rec.warns[0]["bodyARGB"]; !ok {
		t.Errorf("warn must carry bodyARGB even when the body is absent; fields=%v", rec.warns[0])
	}

	api.parseSuperChatInfo(paidMessage("$2.00", float64(2130753748), nil)) // 0x7F00B8D4: same hex, different alpha
	if rec.count() != 2 {
		t.Errorf("a different raw ARGB is a distinct unknown; want 2 warnings, got %d", rec.count())
	}
}

// TestSuperchatColorGivenAsStringStillResolves: this API already sees numeric
// fields flip to strings (videoOffsetTimeMsec); a color delivered as a decimal
// string must resolve like the number. A value of another type is recorded as
// absent and reported at Debug with the raw value, never silently.
func TestSuperchatColorGivenAsStringStillResolves(t *testing.T) {
	rec := &superchatWarnRecorder{}
	api := newSuperchatTestAPI(rec)

	got := api.parseSuperChatInfo(paidMessage("$2.00", "4278237396", nil))
	if got.Tier != 2 || got.Color != "cyan" || got.HeaderColor != "#00B8D4" {
		t.Errorf("string color: got tier %d %q hex %q, want 2 cyan #00B8D4", got.Tier, got.Color, got.HeaderColor)
	}

	got = api.parseSuperChatInfo(paidMessage("$2.00", map[string]any{"odd": true}, nil))
	if got.Tier != 0 || got.Color != "gray" || got.HeaderColor != "" {
		t.Errorf("unparseable color: got tier %d color %q hex %q, want 0 / gray / no hex", got.Tier, got.Color, got.HeaderColor)
	}
	if len(rec.debugs) == 0 || rec.debugs[0] != "chat: superchat color field has an unexpected shape" {
		t.Errorf("unexpected-shape color must be reported at Debug; debugs=%v", rec.debugs)
	}
}

// TestSuperchatRecordJSONShape pins the archived shape of a superchat record:
// a resolved tier carries its palette name; an unmatched one carries tier 0
// and the color "gray", which no palette row uses (the real colors are in the hex fields). No label field:
// tier 0 IS the unknown marker. Readers of chat.json depend on these keys.
func TestSuperchatRecordJSONShape(t *testing.T) {
	api := newSuperchatTestAPI(&superchatWarnRecorder{})

	resolved, err := json.Marshal(api.parseSuperChatInfo(paidMessage("$2.00", float64(4278237396), float64(4278248959))))
	if err != nil {
		t.Fatal(err)
	}
	wantResolved := `{"amount":"$2.00","currency":"USD","color":"cyan","tier":2,"kind":"message","headerColor":"#00B8D4","bodyColor":"#00E5FF"}`
	if string(resolved) != wantResolved {
		t.Errorf("resolved record:\n got %s\nwant %s", resolved, wantResolved)
	}

	unknown, err := json.Marshal(api.parseSuperChatInfo(paidMessage("$3.00", float64(4278190080), nil)))
	if err != nil {
		t.Fatal(err)
	}
	wantUnknown := `{"amount":"$3.00","currency":"USD","color":"gray","tier":0,"kind":"message","headerColor":"#000000"}`
	if string(unknown) != wantUnknown {
		t.Errorf("unknown record:\n got %s\nwant %s", unknown, wantUnknown)
	}
}

// TestSuperchatTierTableHasAHeaderAndABodyColorPerTier pins the shape of the
// single palette table: exactly two opaque colors per tier 1..7 (header and
// body), one name per tier, and no row may use the unknown color "gray" —
// the invariant that makes gray unambiguous.
func TestSuperchatTierTableHasAHeaderAndABodyColorPerTier(t *testing.T) {
	counts := map[int]int{}
	names := map[int]string{}
	for argb, row := range superchatTierColors {
		if row.tier < 1 || row.tier > 7 {
			t.Errorf("%08X: tier %d out of range 1..7", argb, row.tier)
		}
		counts[row.tier]++
		if prev, ok := names[row.tier]; ok && prev != row.color {
			t.Errorf("tier %d has two names: %q and %q", row.tier, prev, row.color)
		}
		names[row.tier] = row.color
		if row.color == superchatUnknownColor {
			t.Errorf("%08X: a palette row uses the unknown color %q", argb, row.color)
		}
		if argb>>24 != 0xFF {
			t.Errorf("%08X: palette keys are opaque (alpha FF)", argb)
		}
	}
	for tier := 1; tier <= 7; tier++ {
		if counts[tier] != 2 {
			t.Errorf("tier %d has %d colors, want 2 (header + body)", tier, counts[tier])
		}
	}
	if len(superchatTierColors) != 14 {
		t.Errorf("table has %d rows, want 14", len(superchatTierColors))
	}
}

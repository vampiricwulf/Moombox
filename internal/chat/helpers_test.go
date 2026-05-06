package chat

import (
	"testing"
)

// TestExtractCurrencyPrefixForms covers the prefix-form fast-paths (USD,
// EUR, GBP, JPY, CAD, AUD) plus the generic prefix-scanner fallback for
// less-common currencies. Audit reports/chat.md TC9.
func TestExtractCurrencyPrefixForms(t *testing.T) {
	tests := []struct {
		amount string
		want   string
	}{
		{"$5.00", "USD"},
		{"$5,000.00", "USD"},
		{"€5,00", "EUR"},
		{"£12.34", "GBP"},
		{"¥500", "JPY"},
		{"CA$5.00", "CAD"},
		{"A$5.00", "AUD"},
		{"BRL 50,00", "BRL"},
		{"MXN 100", "MXN"},
		{"PHP 250.00", "PHP"},
	}
	for _, tc := range tests {
		t.Run(tc.amount, func(t *testing.T) {
			if got := extractCurrency(tc.amount); got != tc.want {
				t.Errorf("extractCurrency(%q): want %q, got %q", tc.amount, tc.want, got)
			}
		})
	}
}

// TestExtractCurrencySuffixForms covers EU/Scandinavian post-fix
// formatting where the symbol trails the digits. The audit's earlier
// implementation returned UNKNOWN for these — this test locks the fix.
func TestExtractCurrencySuffixForms(t *testing.T) {
	tests := []struct {
		amount string
		want   string
	}{
		{"5,00 €", "€"},
		{"100 kr", "kr"},
		{"50.00 zł", "zł"},
	}
	for _, tc := range tests {
		t.Run(tc.amount, func(t *testing.T) {
			got := extractCurrency(tc.amount)
			if got != tc.want {
				t.Errorf("extractCurrency(%q): want %q, got %q", tc.amount, tc.want, got)
			}
		})
	}
}

// TestExtractCurrencyUnknownReturnsSentinel covers the no-currency-
// detected branch.
func TestExtractCurrencyUnknownReturnsSentinel(t *testing.T) {
	tests := []string{
		"",
		"5.00",
		"123",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if got := extractCurrency(in); got != "UNKNOWN" {
				t.Errorf("extractCurrency(%q): want UNKNOWN, got %q", in, got)
			}
		})
	}
}

// TestFormatTimestampUTCStable covers the deterministic UTC formatting
// for live + replay chat. The function intentionally produces UTC
// time-of-day so chat-files replayed by viewers in different timezones
// see the same string. Audit reports/chat.md Q11.
func TestFormatTimestampUTCStable(t *testing.T) {
	api := &ChatAPI{}

	tests := []struct {
		usec string
		want string
	}{
		// 1735689600000000 microseconds = 2025-01-01T00:00:00Z
		{"1735689600000000", "00:00:00"},
		// 1735693200000000 = 2025-01-01T01:00:00Z
		{"1735693200000000", "01:00:00"},
		// Sub-microsecond zero edge case → "0:00:00"
		{"0", "0:00:00"},
		// Unparseable input → "0:00:00" (debug-logged + safe fallback)
		{"not-a-number", "0:00:00"},
		{"", "0:00:00"},
	}
	for _, tc := range tests {
		t.Run(tc.usec, func(t *testing.T) {
			if got := api.formatTimestamp(tc.usec); got != tc.want {
				t.Errorf("formatTimestamp(%q): want %q, got %q", tc.usec, tc.want, got)
			}
		})
	}
}

// TestSelectRendererSuperChatPaidMessageBranch — selectRenderer's
// switch handles 5 distinct renderer types (chatTextMessageRenderer,
// chatPaidMessageRenderer, chatPaidStickerRenderer,
// chatMembershipItemRenderer, chatSponsorshipsGiftPurchaseAnnouncement
// Renderer). Existing tests cover the happy path; this test locks the
// paid-message branch which feeds parseSuperChatInfo.
func TestSelectRendererSuperChatPaidMessageBranch(t *testing.T) {
	item := map[string]any{
		"liveChatPaidMessageRenderer": map[string]any{
			"id":                 "ChwKGkNNWHAtT2J1OFlNREZRRTVGZ2tkOENRSVNLZw",
			"purchaseAmountText": map[string]any{"simpleText": "$5.00"},
		},
	}
	got := selectRenderer(item)
	if got == nil {
		t.Fatal("selectRenderer for paid message: want non-nil renderer")
	}
	if id, _ := got["id"].(string); id == "" {
		t.Errorf("selected renderer should carry the id field")
	}
}

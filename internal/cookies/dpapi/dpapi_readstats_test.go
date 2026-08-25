package dpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"strings"
	"testing"
)

// sealV10 builds a Chrome v10 encrypted_value for plaintext under key.
func sealV10(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	return append([]byte(chromeV10Prefix), append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...)...)
}

// TestDecryptFailuresAreClassified pins the point of the accounting: every
// per-row decrypt failure used to be dropped with no log and no counter, so
// the operator saw only "no relevant cookies in any profile" and could not
// tell App-Bound encryption from a foreign master key from a meta.version
// probe that came back empty. Those need three different responses.
func TestDecryptFailuresAreClassified(t *testing.T) {
	key := newMasterKey(t)
	otherKey := newMasterKey(t)

	tests := []struct {
		name      string
		encrypted []byte
		sentinel  error
		count     func(ChromeReadStats) int
	}{
		{
			name:      "App-Bound v20",
			encrypted: []byte(chromeV20Prefix + "whatever-the-service-holds"),
			sentinel:  ErrAppBoundEncryption,
			count:     func(s ChromeReadStats) int { return s.AppBound },
		},
		{
			name:      "legacy pre-v10 blob",
			encrypted: []byte("\x01\x00\x00\x00legacy-dpapi-blob"),
			sentinel:  ErrLegacyEncryption,
			count:     func(s ChromeReadStats) int { return s.Legacy },
		},
		{
			name:      "another profile's master key",
			encrypted: sealV10(t, otherKey, []byte("SID=value")),
			sentinel:  ErrMasterKeyMismatch,
			count:     func(s ChromeReadStats) int { return s.KeyMismatch },
		},
		{
			name:      "domain hash left on the plaintext",
			encrypted: sealV10(t, key, append(domainHash(".youtube.com"), []byte("SID=value")...)),
			sentinel:  ErrUnusablePlaintext,
			count:     func(s ChromeReadStats) int { return s.UnusablePlain },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// hashPrefix=false is the profile whose meta.version probe came
			// back empty — the case the last fixture models.
			_, err := decryptV10Cookie(key, tt.encrypted, false)
			if err == nil {
				t.Fatal("fixture must not decrypt")
			}
			if !errors.Is(err, tt.sentinel) {
				t.Errorf("error is not classifiable as %v: %v", tt.sentinel, err)
			}

			var stats ChromeReadStats
			stats.Rows = 1
			stats.recordDecryptFailure(err)
			if stats.Failed != 1 {
				t.Errorf("Failed = %d, want 1", stats.Failed)
			}
			if got := tt.count(stats); got != 1 {
				t.Errorf("the failure was not counted under its own reason (got %d, Other=%d)", got, stats.Other)
			}
			if stats.Other != 0 {
				t.Errorf("Other = %d — an unclassified failure is exactly what an operator cannot act on", stats.Other)
			}
		})
	}
}

// TestChromeReadStatsSummary covers the line the operator actually reads.
func TestChromeReadStatsSummary(t *testing.T) {
	if got := (ChromeReadStats{Rows: 10, Decrypted: 10}).Summary(); got != "" {
		t.Errorf("a clean read must produce no summary, got %q", got)
	}

	stats := ChromeReadStats{Rows: 150, Decrypted: 7, Failed: 143, AppBound: 143}
	got := stats.Summary()
	if !strings.Contains(got, "143 of 150") {
		t.Errorf("summary must count the skipped rows against the total, got %q", got)
	}
	if !strings.Contains(got, "App-Bound") {
		t.Errorf("summary must name the reason, got %q", got)
	}

	// A failed meta.version probe is the likeliest cause of unusable
	// plaintext, and saying so is the whole reason the probe outcome is
	// tracked at all.
	probed := ChromeReadStats{Rows: 4, Failed: 4, UnusablePlain: 4, MetaProbeFailed: 1}
	if !strings.Contains(probed.Summary(), "meta.version could not be read") {
		t.Errorf("summary must connect unusable plaintext to the failed probe, got %q", probed.Summary())
	}
	readable := ChromeReadStats{Rows: 4, Failed: 4, UnusablePlain: 4}
	if strings.Contains(readable.Summary(), "meta.version could not be read") {
		t.Errorf("a probe that worked must not be blamed, got %q", readable.Summary())
	}
}

// TestChromeReadStatsAdd covers the multi-profile sweep: the DPAPI fallback
// walks every Chromium profile on the box and reports once.
func TestChromeReadStatsAdd(t *testing.T) {
	var total ChromeReadStats
	total.Add(ChromeReadStats{Rows: 10, Decrypted: 10, MetaVersion: 24})
	total.Add(ChromeReadStats{Rows: 5, Failed: 5, AppBound: 5, MetaProbeFailed: 1})

	if total.Rows != 15 || total.Decrypted != 10 || total.Failed != 5 || total.AppBound != 5 {
		t.Errorf("counts did not compose: %+v", total)
	}
	if total.MetaProbeFailed != 1 {
		t.Errorf("MetaProbeFailed = %d, want 1 — one profile's probe failed", total.MetaProbeFailed)
	}
}

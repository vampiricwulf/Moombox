package config

import (
	"math"
	"runtime"
	"strings"
	"testing"
)

// TestPlatformDefaultsApplyOnlyOnArm64 pins owner ruling R3 (2026-09-24): the
// arm-motivated memory caps — the 256 MB per-job reorder ceiling among them —
// apply ONLY on arm64. The keys exist on every platform; only the defaults
// differ. The seam takes the arch as a PARAMETER precisely so both branches
// are exercised on one host; nothing here depends on where it runs.
//
// MUTANT: keying the branch on strings.HasPrefix(goarch, "arm") — the 32-bit
// "arm" row below then gets 256/1024 and fails. MUTANT: returning the arm
// numbers unconditionally — every non-arm64 row fails. MUTANT: swapping the
// two return values — the arm64 row reports 1024/256.
func TestPlatformDefaultsApplyOnlyOnArm64(t *testing.T) {
	for _, tc := range []struct {
		goarch           string
		perJobMB, budget int
	}{
		{"arm64", 256, 1024},
		{"amd64", 1024, 4096},
		{"arm", 1024, 4096},
		{"386", 1024, 4096},
		{"riscv64", 1024, 4096},
		{"", 1024, 4096},
	} {
		perJobMB, budgetMB := platformDefaults(tc.goarch)
		if perJobMB != tc.perJobMB || budgetMB != tc.budget {
			t.Errorf("platformDefaults(%q) = (%d, %d), want (%d, %d)",
				tc.goarch, perJobMB, budgetMB, tc.perJobMB, tc.budget)
		}
	}
}

// TestDefaultsTakeTheReorderCeilingsFromThisHostsArch is the join: Defaults()
// must go through the seam with runtime.GOARCH rather than freezing one
// platform's numbers into the literal.
//
// MUTANT: drop the platformDefaults call and leave the two fields out of the
// literal — both are 0 here and every assertion fails. MUTANT: call
// platformDefaults("arm64") — on this amd64 host the values read 256/1024 and
// both assertions fail.
func TestDefaultsTakeTheReorderCeilingsFromThisHostsArch(t *testing.T) {
	wantPerJob, wantBudget := platformDefaults(runtime.GOARCH)
	cfg := Defaults()
	if cfg.Downloader.ReorderBufferMB != wantPerJob {
		t.Errorf("Defaults().Downloader.ReorderBufferMB = %d, want %d (platformDefaults(%q))",
			cfg.Downloader.ReorderBufferMB, wantPerJob, runtime.GOARCH)
	}
	if cfg.Downloader.ReorderBudgetMB != wantBudget {
		t.Errorf("Defaults().Downloader.ReorderBudgetMB = %d, want %d (platformDefaults(%q))",
			cfg.Downloader.ReorderBudgetMB, wantBudget, runtime.GOARCH)
	}
}

// TestReorderKeysRejectNegativesAndAcceptZero pins the validation contract: 0
// is the DOCUMENTED "unbounded" value on either key, so only a negative is an
// error, and a negative normalizes back to this host's default.
//
// MUTANT: writing the guard as `< 1` (the shape max_video_resolution used) —
// the two zero rows then report an error and are rewritten to the default,
// which silently deletes an operator's explicit "unbounded".
func TestReorderKeysRejectNegativesAndAcceptZero(t *testing.T) {
	defPerJob, defBudget := platformDefaults(runtime.GOARCH)
	for _, tc := range []struct {
		name             string
		perJob, budget   int
		wantErrSubstring string
		wantPerJob       int
		wantBudget       int
	}{
		{"zero on both is unbounded, not an error", 0, 0, "", 0, 0},
		{"a large pair is accepted as written", 8192, 16384, "", 8192, 16384},
		{"negative per-job resets", -1, 4096, "downloader.reorder_buffer_mb", defPerJob, 4096},
		{"negative budget resets", 1024, -1, "downloader.reorder_budget_mb", 1024, defBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Downloader.ReorderBufferMB = tc.perJob
			cfg.Downloader.ReorderBudgetMB = tc.budget

			reported := ""
			for _, err := range Validate(cfg) {
				if strings.Contains(err.Error(), "reorder_") {
					reported = err.Error()
				}
			}
			if tc.wantErrSubstring == "" && reported != "" {
				t.Errorf("Validate reported %q for a legal pair (%d, %d)", reported, tc.perJob, tc.budget)
			}
			if tc.wantErrSubstring != "" && !strings.Contains(reported, tc.wantErrSubstring) {
				t.Errorf("Validate reported %q, want it to name %q", reported, tc.wantErrSubstring)
			}

			Normalize(cfg)
			if cfg.Downloader.ReorderBufferMB != tc.wantPerJob {
				t.Errorf("after Normalize ReorderBufferMB = %d, want %d", cfg.Downloader.ReorderBufferMB, tc.wantPerJob)
			}
			if cfg.Downloader.ReorderBudgetMB != tc.wantBudget {
				t.Errorf("after Normalize ReorderBudgetMB = %d, want %d", cfg.Downloader.ReorderBudgetMB, tc.wantBudget)
			}
		})
	}
}

// TestReorderLimitBytesClampsOnlyWhenBothAreBounded pins the read-time clamp
// R3 asks for: a per-job ceiling ABOVE the process-wide budget is meaningless
// (the budget would refuse the bytes the per-job value promised), so it is
// clamped to the budget and the caller is told, once, so it can warn.
//
// MUTANT: dropping the `both non-zero` condition — the (0, 1024) row then
// reports clamped and returns 1 GiB for a per-job value the operator
// explicitly set to unbounded. MUTANT: clamping the budget UP to the per-job
// value instead — the (4096, 1024) row returns 4 GiB for the total. MUTANT:
// dropping the <<20 — every byte figure is 2^20 times too small.
func TestReorderLimitBytesClampsOnlyWhenBothAreBounded(t *testing.T) {
	const mb = 1 << 20
	for _, tc := range []struct {
		name                  string
		perJobMB, budgetMB    int
		wantPerJob, wantTotal int
		wantClamped           bool
	}{
		{"the amd64 defaults pass through", 1024, 4096, 1024 * mb, 4096 * mb, false},
		{"the arm64 defaults pass through", 256, 1024, 256 * mb, 1024 * mb, false},
		{"per-job above the budget is clamped", 4096, 1024, 1024 * mb, 1024 * mb, true},
		{"per-job equal to the budget is not clamped", 1024, 1024, 1024 * mb, 1024 * mb, false},
		{"an unbounded per-job is never clamped", 0, 1024, 0, 1024 * mb, false},
		{"an unbounded budget never clamps", 4096, 0, 4096 * mb, 0, false},
		{"both unbounded", 0, 0, 0, 0, false},
		{"an absurd value saturates instead of overflowing", (math.MaxInt >> 20) + 1, 0, math.MaxInt, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DownloaderConfig{ReorderBufferMB: tc.perJobMB, ReorderBudgetMB: tc.budgetMB}
			perJob, total, clamped := d.ReorderLimitBytes()
			if perJob != tc.wantPerJob || total != tc.wantTotal || clamped != tc.wantClamped {
				t.Errorf("ReorderLimitBytes() = (%d, %d, %v), want (%d, %d, %v)",
					perJob, total, clamped, tc.wantPerJob, tc.wantTotal, tc.wantClamped)
			}
		})
	}
}

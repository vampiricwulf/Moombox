package database

import "testing"

func TestPrecisionRank(t *testing.T) {
	// Spec §6/§12: assumed < coarse < day < exact < started; unknown strings rank
	// as assumed (fail-closed: a typo loses to everything rather than winning).
	want := map[string]int{"assumed": 1, "coarse": 2, "day": 3, "exact": 4, "started": 5, "": 1, "exactt": 1}
	for p, w := range want {
		if got := PrecisionRank(p); got != w {
			t.Errorf("PrecisionRank(%q) = %d, want %d", p, got, w)
		}
	}
}

func TestPrecisionRankCaseSQLMatchesGo(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for _, p := range []string{"assumed", "coarse", "day", "exact", "started", "bogus"} {
		var sqlRank int
		q := `SELECT ` + precisionRankCaseSQL("?")
		if err := db.db.QueryRow(q, p).Scan(&sqlRank); err != nil {
			t.Fatal(err)
		}
		if sqlRank != PrecisionRank(p) {
			t.Errorf("SQL rank(%q)=%d, Go rank=%d — the ladder MUST have one definition", p, sqlRank, PrecisionRank(p))
		}
	}
}

package database

// PrecisionRank orders the date-precision ladder (spec §12). One definition:
// both SQL statements render their CASE from precisionRankCaseSQL below.
// Unrecognised strings rank as 'assumed' — fail-closed (spec §6).
func PrecisionRank(p string) int {
	switch p {
	case "started":
		return 5
	case "exact":
		return 4
	case "day":
		return 3
	case "coarse":
		return 2
	default:
		return 1
	}
}

// precisionRankCaseSQL renders the ladder as a SQL CASE over expr.
func precisionRankCaseSQL(expr string) string {
	return "CASE " + expr +
		" WHEN 'started' THEN 5 WHEN 'exact' THEN 4 WHEN 'day' THEN 3" +
		" WHEN 'coarse' THEN 2 ELSE 1 END"
}

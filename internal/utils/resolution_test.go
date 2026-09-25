package utils

import "testing"

// TestCapDimension pins WHICH dimension max_video_resolution compares against.
// Ruling R1: the SHORTER one, so 2160 recognises 4K whichever way a stream
// orders its dimensions.
//
// Mutant: returning max(width, height) — the rule all four selection sites
// used before this arc. Rows 1 and 2 both fail (3840 twice, want 2160 twice).
func TestCapDimension(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		want          int
	}{
		{"landscape 4K reports its height", 3840, 2160, 2160},
		{"portrait 4K reports its width", 2160, 3840, 2160},
		{"square", 1080, 1080, 1080},
		{"width only", 1920, 0, 1920},
		{"height only", 0, 1080, 1080},
		{"no dimensions at all", 0, 0, 0},
		{"a non-positive dimension counts as absent", -1, 720, 720},
		{"both non-positive", -4, -2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapDimension(tc.width, tc.height); got != tc.want {
				t.Errorf("CapDimension(%d, %d) = %d, want %d", tc.width, tc.height, got, tc.want)
			}
		})
	}
}

// TestSelectByCap pins the whole of R1's selection rule: the largest candidate
// at or below the cap; failing that the SMALLEST one above it (the cap is a
// preference, never an exclusion that leaves nothing); 0 is unbounded and
// always picks the largest; an empty list selects nothing.
//
// Mutants, one per group below:
//   - long edge restored: "the 4K ladder at the shipped default" resolves to
//     1080 instead of 2160, and "a portrait 4K source" to 0/false.
//   - closest-above replaced by largest: "nothing at or below" returns 2160.
//   - closest-above preferred over at-or-below: "something below outranks
//     something above" returns 1440 instead of 720.
//   - capPx <= 0 treated as a literal ceiling: "unbounded picks the largest"
//     returns 0/false.
func TestSelectByCap(t *testing.T) {
	ladder := []Cand{
		{Width: 640, Height: 360},
		{Width: 1280, Height: 720},
		{Width: 1920, Height: 1080},
		{Width: 2560, Height: 1440},
		{Width: 3840, Height: 2160},
	}

	for _, tc := range []struct {
		name       string
		capPx      int
		candidates []Cand
		wantSize   int
		wantOK     bool
	}{
		{"the 4K ladder at the shipped default", 2160, ladder, 2160, true},
		{"the 4K ladder at 1080", 1080, ladder, 1080, true},
		{"a cap above everything offered", 4320, ladder, 2160, true},
		{"unbounded picks the largest", 0, ladder, 2160, true},
		{
			name:       "a portrait 4K source is admitted by a 2160 cap",
			capPx:      2160,
			candidates: []Cand{{Width: 2160, Height: 3840}},
			wantSize:   2160,
			wantOK:     true,
		},
		{
			name:       "nothing at or below picks the closest above",
			capPx:      1080,
			candidates: []Cand{{Width: 2560, Height: 1440}, {Width: 3840, Height: 2160}},
			wantSize:   1440,
			wantOK:     true,
		},
		{
			name:       "something below outranks something above",
			capPx:      1080,
			candidates: []Cand{{Width: 1280, Height: 720}, {Width: 2560, Height: 1440}},
			wantSize:   720,
			wantOK:     true,
		},
		{
			name:       "a dimensionless candidate never outranks a sized one",
			capPx:      0,
			candidates: []Cand{{Width: 0, Height: 0}, {Width: 1920, Height: 1080}},
			wantSize:   1080,
			wantOK:     true,
		},
		{"no candidates, capped", 2160, nil, 0, false},
		{"no candidates, unbounded", 0, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SelectByCap(tc.capPx, tc.candidates)
			if ok != tc.wantOK {
				t.Fatalf("SelectByCap(%d, %v) ok = %v, want %v", tc.capPx, tc.candidates, ok, tc.wantOK)
			}
			if got != tc.wantSize {
				t.Errorf("SelectByCap(%d, %v) = %d, want %d", tc.capPx, tc.candidates, got, tc.wantSize)
			}
		})
	}
}

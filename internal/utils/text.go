package utils

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// NormalizeText normalizes text by applying NFD decomposition, removing
// diacritics, converting to lowercase, and normalizing whitespace.
func NormalizeText(s string) string {
	// NFD decomposition
	decomposed := norm.NFD.String(s)

	// Remove diacritical marks (Mn category)
	var b strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}

	// Lowercase and normalize whitespace
	result := strings.ToLower(b.String())
	result = strings.Join(strings.Fields(result), " ")
	return result
}

// FuzzyMatch performs a fuzzy substring match after normalizing both strings.
func FuzzyMatch(haystack, needle string) bool {
	normalizedHaystack := NormalizeText(haystack)
	normalizedNeedle := NormalizeText(needle)
	return strings.Contains(normalizedHaystack, normalizedNeedle)
}

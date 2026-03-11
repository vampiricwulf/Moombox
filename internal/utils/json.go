package utils

import (
	"bytes"
	"strconv"
	"strings"
)

const messageCountPadWidth = 20 // Fixed-width padding for messageCount in JSON header

// PadMessageCountJSON pads the messageCount numeric value in serialized JSON to
// a fixed width with trailing whitespace. This ensures the header byte size stays
// constant during in-place updates as the count grows.
func PadMessageCountJSON(data []byte) []byte {
	marker := []byte(`"messageCount":`)
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return data
	}
	pos := idx + len(marker)
	for pos < len(data) && (data[pos] == ' ' || data[pos] == '\t') {
		pos++
	}
	numStart := pos
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		pos++
	}
	if pos == numStart {
		return data
	}

	padNeeded := messageCountPadWidth - (pos - numStart)
	if padNeeded <= 0 {
		return data
	}

	result := make([]byte, len(data)+padNeeded)
	copy(result, data[:pos])
	for i := range padNeeded {
		result[pos+i] = ' '
	}
	copy(result[pos+padNeeded:], data[pos:])
	return result
}

// ReplaceMessageCount replaces the messageCount value in a JSON header string
// with the new count padded to fixed width. Handles both legacy (unpadded) and
// padded files by scanning digits + trailing whitespace as the old value field.
func ReplaceMessageCount(header string, count int) string {
	mcStart := strings.Index(header, `"messageCount":`)
	if mcStart < 0 {
		return header
	}

	pos := mcStart + len(`"messageCount":`)
	for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t') {
		pos++
	}
	numStart := pos
	for pos < len(header) && header[pos] >= '0' && header[pos] <= '9' {
		pos++
	}
	if pos == numStart {
		return header
	}
	// Also scan trailing whitespace (from previous padding)
	for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t') {
		pos++
	}

	oldFieldWidth := pos - numStart
	newVal := strconv.Itoa(count)
	targetWidth := max(messageCountPadWidth, oldFieldWidth)
	padded := newVal + strings.Repeat(" ", max(0, targetWidth-len(newVal)))

	return header[:numStart] + padded + header[pos:]
}

// ReplaceQuotedField replaces a JSON string value in a header buffer.
// Finds `"key": "old_value"` and replaces with `"key": "new_value"`.
func ReplaceQuotedField(header *string, key, newValue string) {
	h := *header
	keyIdx := strings.Index(h, key)
	if keyIdx < 0 {
		return
	}
	// Find opening quote after key
	valStart := keyIdx + len(key)
	for valStart < len(h) && h[valStart] != '"' {
		valStart++
	}
	if valStart >= len(h) {
		return
	}
	valStart++ // skip opening quote
	// Find closing quote
	valEnd := valStart
	for valEnd < len(h) && h[valEnd] != '"' {
		valEnd++
	}
	if valEnd >= len(h) {
		return
	}
	*header = h[:valStart] + newValue + h[valEnd:]
}

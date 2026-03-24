package utils

import (
	"strings"
	"testing"
)

func TestPadMessageCountJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int // expected total width of the numeric field after padding
	}{
		{
			name:  "small count gets padded",
			input: `{"messageCount": 42, "messages": []}`,
			want:  messageCountPadWidth,
		},
		{
			name:  "zero gets padded",
			input: `{"messageCount": 0, "messages": []}`,
			want:  messageCountPadWidth,
		},
		{
			name:  "no messageCount field",
			input: `{"otherField": 42}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := PadMessageCountJSON([]byte(tt.input))
			resultStr := string(result)

			if tt.want == 0 {
				// No padding expected
				if resultStr != tt.input {
					t.Errorf("expected no change, got %q", resultStr)
				}
				return
			}

			// Find the count value + padding
			found := strings.Contains(resultStr, `"messageCount":`)
			if !found {
				t.Fatal("messageCount field not found in result")
			}
			// The padded field should make the total length increase
			if len(result) <= len(tt.input) {
				t.Error("expected output to be longer than input due to padding")
			}
		})
	}
}

func TestReplaceMessageCount(t *testing.T) {
	tests := []struct {
		name   string
		header string
		count  int
		check  string // substring that should appear in result
	}{
		{
			name:   "replace count",
			header: `{"messageCount": 42                  , "other": "val"}`,
			count:  100,
			check:  "100",
		},
		{
			name:   "no messageCount field",
			header: `{"other": "val"}`,
			count:  100,
			check:  `{"other": "val"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReplaceMessageCount(tt.header, tt.count)
			if !strings.Contains(result, tt.check) {
				t.Errorf("result %q does not contain %q", result, tt.check)
			}
		})
	}
}

func TestReplaceQuotedField(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		key      string
		newValue string
		want     string
	}{
		{
			name:     "replace downloadedAt",
			header:   `{"downloadedAt": "2024-01-01T00:00:00Z", "other": "val"}`,
			key:      `"downloadedAt":`,
			newValue: "2024-06-15T12:00:00Z",
			want:     `{"downloadedAt": "2024-06-15T12:00:00Z", "other": "val"}`,
		},
		{
			name:     "key not found",
			header:   `{"other": "val"}`,
			key:      `"downloadedAt":`,
			newValue: "new",
			want:     `{"other": "val"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := tt.header
			ReplaceQuotedField(&header, tt.key, tt.newValue)
			if header != tt.want {
				t.Errorf("got %q, want %q", header, tt.want)
			}
		})
	}
}

func TestPadMessageCountJSONAlreadyPadded(t *testing.T) {
	// A count that's already at or beyond padWidth should not be changed
	longNum := strings.Repeat("9", messageCountPadWidth+1)
	input := `{"messageCount": ` + longNum + `, "messages": []}`
	result := PadMessageCountJSON([]byte(input))
	if string(result) != input {
		t.Errorf("expected no change for already-wide count, got %q", string(result))
	}
}

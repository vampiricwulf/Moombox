package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FlexDuration represents a time value that can be specified as either a plain
// number (in a unit determined by context) or as a duration string like "10m", "7d".
// When used as feed_check_interval, the numeric value is in minutes.
// When used as hide_finished_age_days, the numeric value is in days.
type FlexDuration struct {
	Value float64
}

var durationPattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(ms|s|m|h|d|w)$`)

// Multipliers convert to milliseconds.
var durationMultipliers = map[string]float64{
	"ms": 1,
	"s":  1_000,
	"m":  60_000,
	"h":  3_600_000,
	"d":  86_400_000,
	"w":  604_800_000,
}

// ParseFlexDuration parses a value that can be a number or a duration string.
// The unit parameter specifies what the numeric value represents ("minutes" or "days").
// The result is always stored as the numeric value in its natural unit.
// Negative values are rejected and the default is used instead.
func ParseFlexDuration(value interface{}, unit string, defaultValue float64) FlexDuration {
	var result FlexDuration
	switch v := value.(type) {
	case int64:
		result = FlexDuration{Value: float64(v)}
	case float64:
		result = FlexDuration{Value: v}
	case int:
		result = FlexDuration{Value: float64(v)}
	case string:
		result = parseStringDuration(v, unit, defaultValue)
	default:
		return FlexDuration{Value: defaultValue}
	}
	// Reject negative values — return default instead
	if result.Value < 0 {
		return FlexDuration{Value: defaultValue}
	}
	return result
}

func parseStringDuration(s string, unit string, defaultValue float64) FlexDuration {
	s = strings.TrimSpace(s)

	// Try plain number first
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return FlexDuration{Value: n}
	}

	// Try duration string
	matches := durationPattern.FindStringSubmatch(strings.ToLower(s))
	if matches == nil {
		return FlexDuration{Value: defaultValue}
	}

	num, _ := strconv.ParseFloat(matches[1], 64)
	mult, ok := durationMultipliers[matches[2]]
	if !ok {
		return FlexDuration{Value: defaultValue}
	}

	ms := num * mult

	// Convert from milliseconds to the target unit
	switch unit {
	case "minutes":
		return FlexDuration{Value: ms / 60_000}
	case "days":
		return FlexDuration{Value: ms / 86_400_000}
	default:
		return FlexDuration{Value: defaultValue}
	}
}

// MarshalJSON serializes FlexDuration as a plain number so the API returns
// e.g. 30 instead of {"Value":30}. This matches what the frontend expects.
func (d FlexDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Value)
}

// UnmarshalJSON deserializes FlexDuration from a plain number.
func (d *FlexDuration) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &d.Value)
}

// Minutes returns the value interpreted as minutes.
func (d FlexDuration) Minutes() float64 {
	return d.Value
}

// Days returns the value interpreted as days.
func (d FlexDuration) Days() float64 {
	return d.Value
}

// UnmarshalTOML implements the TOML unmarshaler interface.
// For plain numbers, the value is stored as-is (caller knows the unit context).
// For duration strings (e.g. "7d", "30m"), the value is stored in the string's unit
// using a best-effort heuristic: if the suffix is d/w, store as days; else as minutes.
// Also handles map input from TOML tables (e.g. when a previously-saved config wrote
// FlexDuration as {Value = 5.0}).
func (d *FlexDuration) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case int64:
		d.Value = float64(v)
	case float64:
		d.Value = v
	case string:
		s := strings.TrimSpace(v)
		// Plain number: store as-is
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			d.Value = n
			return nil
		}
		// Duration string: detect unit from suffix
		matches := durationPattern.FindStringSubmatch(strings.ToLower(s))
		if matches != nil {
			suffix := matches[2]
			// Days/weeks fields → parse as days; everything else → parse as minutes
			if suffix == "d" || suffix == "w" {
				*d = parseStringDuration(s, "days", 0)
			} else {
				*d = parseStringDuration(s, "minutes", 0)
			}
			return nil
		}
		return fmt.Errorf("invalid duration %q: expected number or duration string (e.g. \"10m\", \"7d\")", s)
	case map[string]interface{}:
		// Handle TOML table form: {Value = 5.0} (written by toml.Encoder for struct)
		if val, ok := v["Value"]; ok {
			return d.UnmarshalTOML(val)
		}
		d.Value = 0
	default:
		return fmt.Errorf("unsupported type for FlexDuration: %T", data)
	}
	return nil
}

// MarshalTOML serializes FlexDuration as a plain number in TOML output,
// preventing the encoder from writing it as a [table] with a Value key.
func (d FlexDuration) MarshalTOML() ([]byte, error) {
	s := strconv.FormatFloat(d.Value, 'f', -1, 64)
	return []byte(s), nil
}

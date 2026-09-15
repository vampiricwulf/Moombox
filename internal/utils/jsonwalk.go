package utils

import (
	"encoding/json"
	"fmt"
)

// SkipJSONValue consumes exactly one value from dec — scalar, object or array —
// leaving the decoder positioned after it. For walking a JSON object's keys
// without materialising the values you do not want.
func SkipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil
	}
	if delim != '{' && delim != '[' {
		return fmt.Errorf("unexpected %q where a value was expected", delim)
	}
	return FinishJSONValue(dec, 1)
}

// FinishJSONValue walks tokens until the given open-composite depth closes.
// Pass 1 just after consuming an opening '{' or '['.
func FinishJSONValue(dec *json.Decoder, depth int) error {
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, isDelim := tok.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

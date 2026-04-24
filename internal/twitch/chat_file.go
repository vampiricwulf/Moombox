package twitch

import (
	"encoding/json"
	"fmt"
	"os"
)

// readChatFileMessages reads an existing Twitch chat file and returns its
// Messages array. Used by the flush path's recovery fallback when an
// incremental append fails — the caller can merge these with the current
// batch and rewrite the full file without dropping prior data.
func readChatFileMessages(path string) ([]TwitchChatMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var existing TwitchChatData
	if err := json.Unmarshal(raw, &existing); err != nil {
		return nil, fmt.Errorf("parse existing chat file: %w", err)
	}
	return existing.Messages, nil
}

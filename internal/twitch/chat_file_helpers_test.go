package twitch

import (
	"encoding/json"
	"fmt"
	"os"
)

// readChatFileMessages reads a Twitch chat file whole and returns its Messages
// array — a test helper now. The write paths' fallback reads history through
// utils.SalvageChatMessages instead, which keeps the intact messages of a
// damaged file where this fails outright.
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

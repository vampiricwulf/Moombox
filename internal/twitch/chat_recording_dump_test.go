package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDumpLostChatBatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "channel - part1.json")
	batch := []map[string]any{{"text": "hello"}, {"text": "world"}}

	if err := dumpLostChatBatch(p, batch); err != nil {
		t.Fatalf("dumpLostChatBatch: %v", err)
	}

	data, err := os.ReadFile(p + ".lostbatch.json")
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("recovered %d messages, want 2", len(out))
	}
}

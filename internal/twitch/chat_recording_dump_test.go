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

// A part can spill twice — resumed after a restart, or rolled with a spill
// and then ended with another — and the second spill used to overwrite the
// first's messages. A file at that name that is not a spill is kept aside.
//
// Mutants: writing the batch alone (the first spill's message is gone), and
// overwriting an unreadable file instead of keeping it.
func TestDumpLostChatBatchKeepsAnEarlierSpill(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat.json")
	if err := dumpLostChatBatch(p, []map[string]any{{"text": "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := dumpLostChatBatch(p, []map[string]any{{"text": "second"}, {"text": "third"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p + ".lostbatch.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("spill is not valid JSON: %v", err)
	}
	if len(out) != 3 || out[0]["text"] != "first" || out[2]["text"] != "third" {
		t.Errorf("spill holds %v, want first, second, third", out)
	}

	q := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(q+".lostbatch.json", []byte("not a spill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dumpLostChatBatch(q, []map[string]any{{"text": "new"}}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(q + ".lostbatch.json.*")
	if len(matches) != 1 {
		t.Fatalf("aside files = %v, want the unreadable one kept", matches)
	}
	if kept, _ := os.ReadFile(matches[0]); string(kept) != "not a spill" {
		t.Errorf("aside file holds %q", kept)
	}
}

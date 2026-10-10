package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cutTestMessage ends with its own array, as a chat message does ("message").
type cutTestMessage struct {
	ID      string   `json:"id"`
	Message []string `json:"message"`
}

type cutTestFile struct {
	VideoID      string           `json:"videoId"`
	MessageCount int              `json:"messageCount"`
	Messages     []cutTestMessage `json:"messages"`
}

// TestChatFileEndRecognisesOnlyTheMessagesArray: a file cut right after a
// message's closing brace ends with that message's own ']' and a '}' — the
// shape the first end check accepted, so an append wrote the batch into the
// message and reported success over a file that no longer parsed. Only the
// messages array's own ']' (the writers' "\n  ]", or "[]" when empty) closes
// the document.
//
// Mutant: drop the `before` check in closesChatDocument — both cut layouts
// read as intact and the append succeeds.
func TestChatFileEndRecognisesOnlyTheMessagesArray(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, msgs []cutTestMessage) string {
		path := filepath.Join(dir, name)
		if err := WriteChatFileAtomic(path, cutTestFile{VideoID: "v", MessageCount: len(msgs), Messages: msgs}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	two := []cutTestMessage{{ID: "a", Message: []string{"x"}}, {ID: "b", Message: []string{"y"}}}

	// The writers' own layouts are intact: indented, empty, and appended to.
	indented := write("indented.json", two)
	empty := write("empty.json", []cutTestMessage{})
	appended := write("appended.json", two)
	if err := AppendChatMessages(appended, []cutTestMessage{{ID: "c", Message: []string{"z"}}}, 3, nil); err != nil {
		t.Fatalf("append to a whole file: %v", err)
	}
	for _, path := range []string{indented, empty, appended} {
		if ok, err := ChatFileEndIntact(path); err != nil || !ok {
			t.Errorf("%s: intact = %v, %v; want true", filepath.Base(path), ok, err)
		}
	}

	// Cut right after a message: the indented layout ("\n      ]\n    }")
	// and the appended, compact one ("]}").
	cutAfter := func(name, src, end string) string {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		i := strings.LastIndex(string(raw), end)
		if i < 0 {
			t.Fatalf("%q not in %s", end, src)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw[:i+len(end)], 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, path := range []string{
		cutAfter("cut-indented.json", indented, "\n      ]\n    }"),
		cutAfter("cut-compact.json", appended, `"z"]}`),
	} {
		if ok, err := ChatFileEndIntact(path); err != nil || ok {
			t.Errorf("%s: intact = %v, %v; want false", filepath.Base(path), ok, err)
		}
		before, _ := os.ReadFile(path)
		err := AppendChatMessages(path, []cutTestMessage{{ID: "d", Message: []string{"w"}}}, 9, nil)
		if !errors.Is(err, ErrChatFileDamaged) {
			t.Errorf("%s: append = %v, want ErrChatFileDamaged", filepath.Base(path), err)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("%s: the refused append changed the file", filepath.Base(path))
		}
	}
}

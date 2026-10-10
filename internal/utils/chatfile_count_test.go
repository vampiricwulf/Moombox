package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadChatFileMessageCount(t *testing.T) {
	dir := t.TempDir()
	written := filepath.Join(dir, "written.json")
	if err := WriteChatFileAtomic(written, map[string]any{"messageCount": 12, "messages": []int{}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendChatMessages(written, []map[string]string{{"id": "a"}}, 13, nil); err != nil {
		t.Fatal(err)
	}
	noCount := filepath.Join(dir, "nocount.json")
	if err := os.WriteFile(noCount, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		path   string
		want   int
		wantOK bool
	}{
		{"padded header after an append", written, 13, true},
		{"no count in the header", noCount, 0, false},
		{"missing file", filepath.Join(dir, "absent.json"), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ReadChatFileMessageCount(tc.path)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("got %d, %v; want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

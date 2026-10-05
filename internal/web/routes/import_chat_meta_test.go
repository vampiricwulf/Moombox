package routes

import (
	"io"
	"strings"
	"testing"
)

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The import read a chat archive whole — hundreds of MB for a long stream —
// to pull three header strings out of it. It now stops once it has them, so
// a 10 MB messages array after the header is never read.
//
// Mutant: reading the stream to the end (io.ReadAll + json.Unmarshal) — the
// whole archive is read.
func TestImportChatMetaStopsAfterTheHeader(t *testing.T) {
	header := `{"videoId":"abc123def45","videoTitle":"Title","channelName":"Chan","messages":[`
	body := strings.Repeat(`{"id":"m","message":"hello there"},`, 300_000) + `{"id":"last"}]}`
	cr := &countingReader{r: io.MultiReader(strings.NewReader(header), strings.NewReader(body))}

	got := readImportChatMeta(cr)
	if got != (importChatMeta{VideoID: "abc123def45", VideoTitle: "Title", ChannelName: "Chan"}) {
		t.Errorf("meta = %+v", got)
	}
	if cr.n > 64<<10 {
		t.Errorf("read %d bytes of a %d-byte archive for its header", cr.n, len(header)+len(body))
	}
}

func TestImportChatMetaShapes(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     importChatMeta
	}{
		{"messages first, nested", `{"messages":[{"a":[1,2,{"b":"videoTitle"}]}],"videoTitle":"T"}`, importChatMeta{VideoTitle: "T"}},
		{"values of the wrong type", `{"videoId":5,"videoTitle":{"x":"y"},"channelName":"C"}`, importChatMeta{ChannelName: "C"}},
		{"not an object", `[1,2,3]`, importChatMeta{}},
		{"truncated", `{"videoTitle":"T","messages":[{"a"`, importChatMeta{VideoTitle: "T"}},
		{"empty", ``, importChatMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readImportChatMeta(strings.NewReader(tc.in)); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

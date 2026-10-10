package routes

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// longJSONString is a JSON string token of n bytes of 'a', quotes included,
// as a reader: the test never holds it.
func longJSONString(n int64) io.Reader {
	return io.MultiReader(strings.NewReader(`"`), io.LimitReader(repeatByteReader('a'), n-2), strings.NewReader(`"`))
}

// repeatByteReader reads as an endless run of one byte.
type repeatByteReader byte

func (r repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// The import's JSON reads went as a stream but held each token whole, as
// json.Decoder does: one 96 MB string in a ".json" the import does not even
// keep cost 350 MB of heap to classify, and a kept chat's header read did the
// same with a long title. Neither read now takes in more than
// importJSONTokenLimit past the last token it returned, and a longer token
// ends it: the entry is no chat, and the header keeps what came before.
//
// Mutant: importJSONSource.Read ignoring its limit (both reads take in the
// whole string — 32 MiB — and the header gets the title and channel after
// it, the classification the messages).
func TestImportJSONReadsHoldNoTokenPastTheLimit(t *testing.T) {
	const strLen = 32 << 20
	entry := func() *countingReader {
		return &countingReader{r: io.MultiReader(
			strings.NewReader(`{"videoId":"dQw4w9WgXcQ","videoTitle":`),
			longJSONString(strLen),
			strings.NewReader(`,"channelName":"Chan","messages":[{"offsetMs":1}]}`),
		)}
	}
	// What a read may take in: the limit past the last token, and what the
	// decoder's last refill asked for beyond it.
	const most = importJSONTokenLimit + 64<<10

	cr := entry()
	if got := readImportChatMeta(cr); got != (importChatMeta{VideoID: "dQw4w9WgXcQ"}) {
		t.Errorf("header id %q title %.40q (%d bytes) channel %q, want the id ahead of the long title and nothing after it",
			got.VideoID, got.VideoTitle, len(got.VideoTitle), got.ChannelName)
	}
	if cr.n > most {
		t.Errorf("the header read took in %d bytes of a %d-byte title", cr.n, strLen)
	}

	cr = entry()
	if importJSONHoldsChat(cr) {
		t.Error("a .json whose messages follow a token past the limit was read to them")
	}
	if cr.n > most {
		t.Errorf("the chat check took in %d bytes of a %d-byte title", cr.n, strLen)
	}
}

// The limit is on one token, not on the read: a Twitch chat's emotes, ahead
// of its messages, run to megabytes of small tokens for a channel with
// thousands, and a chat whose header follows its messages is read past them
// (TestImportChatMetaShapes). Both read to the end they did before.
//
// Mutant: the limit counted from the start of the entry rather than from
// the last token (allow setting it to importJSONTokenLimit): the emotes end
// the chat check, and the messages the header read.
func TestImportJSONTokenLimitIsPerToken(t *testing.T) {
	var emotes strings.Builder
	for i := 0; emotes.Len() < 3*importJSONTokenLimit; i++ {
		if i > 0 {
			emotes.WriteString(",")
		}
		fmt.Fprintf(&emotes, `{"id":"%d","code":"Emote%d","url":"https://cdn.7tv.app/emote/%d/1x.webp"}`, i, i, i)
	}
	twitch := `{"platform":"twitch","channelLogin":"somestreamer","channelDisplayName":"SomeStreamer",` +
		`"emotes":{"seventv":[` + emotes.String() + `]},"messages":[{"offsetMs":1}]}`
	if !importJSONHoldsChat(strings.NewReader(twitch)) {
		t.Errorf("a Twitch chat with %d bytes of emotes ahead of its messages is not a chat", emotes.Len())
	}

	var messages strings.Builder
	for i := 0; messages.Len() < 3*importJSONTokenLimit; i++ {
		fmt.Fprintf(&messages, `{"offsetMs":%d,"message":"an ordinary chat line, number %d"},`, i*100, i)
	}
	trailing := `{"messages":[` + strings.TrimSuffix(messages.String(), ",") + `],"videoId":"dQw4w9WgXcQ","videoTitle":"T","channelName":"C"}`
	if got := readImportChatMeta(strings.NewReader(trailing)); got != (importChatMeta{VideoID: "dQw4w9WgXcQ", VideoTitle: "T", ChannelName: "C"}) {
		t.Errorf("header %+v after %d bytes of messages", got, messages.Len())
	}
}

// Through the route: a zip of one video, its chat with a 24 MiB title, and a
// "notes.json" — kept by nothing — whose one value is a 24 MiB string ahead
// of a messages array. The import keeps the video and the chat, takes the
// title from the file name, does not call notes.json a chat, and holds
// neither string to find that out: it allocated some 3.7 times each before.
//
// Mutant: importJSONSource.Read ignoring its limit (notes.json is listed as
// a left-out chat, the row takes the long title, and the request allocates
// some 180 MB).
func TestImportHoldsNoLongJSONTokenOfAnEntry(t *testing.T) {
	stubImportDurations(t)
	f := newImportFixture(t)
	const strLen = 24 << 20
	long := func(w io.Writer, prefix, suffix string) {
		t.Helper()
		if _, err := io.WriteString(w, prefix); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, longJSONString(strLen)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, suffix); err != nil {
			t.Fatal(err)
		}
	}
	// Random stored bytes keep the zip's ratio under the import's 100.
	pad := make([]byte, 600<<10)
	if _, err := rand.Read(pad); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "Stream [dQw4w9WgXcQ].mp4", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pad); err != nil {
		t.Fatal(err)
	}
	if w, err = zw.Create("Stream [dQw4w9WgXcQ].chat.json"); err != nil {
		t.Fatal(err)
	}
	long(w, `{"videoId":"dQw4w9WgXcQ","videoTitle":`, `,"channelName":"Chan","messages":[{"offsetMs":1}]}`)
	if w, err = zw.Create("notes.json"); err != nil {
		t.Fatal(err)
	}
	long(w, `{"title":`, `,"messages":[{"offsetMs":1}]}`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rec, job := importZip(t, f, buf.Bytes())
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if r := decodeImportResult(t, rec.Body.Bytes()); len(r.Import.UnpairedChats) != 0 {
		t.Errorf("left-out chats %q: notes.json was read past its long string", r.Import.UnpairedChats)
	}
	if job.Title != "Stream" || job.ChannelName != "Import" || job.ChatFilename == "" {
		t.Errorf("title %.40q channel %q chat %.60q: want the file name's title, no channel, the chat kept", job.Title, job.ChannelName, job.ChatFilename)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > strLen {
		t.Errorf("the import allocated %d MiB for two %d MiB strings neither read needs", alloc>>20, strLen>>20)
	}
}

package routes

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// abandonGate stands in for an import request's context and holds the route
// at its first look at it — the client check just before the insert, once the
// archive is extracted — until the test opens it; it then answers with the
// real context's verdict. It orders the events and never decides them: the
// client is gone only when the test's connection really closed.
type abandonGate struct {
	context.Context // the request's own; set by the handler before the route runs
	held, opened    sync.Once
	reached         chan struct{} // closed once the route asks
	release         chan struct{} // closed by open
}

func (g *abandonGate) Err() error {
	g.held.Do(func() {
		close(g.reached)
		<-g.release
	})
	return g.Context.Err()
}

func (g *abandonGate) open() { g.opened.Do(func() { close(g.release) }) }

// codeWriter records the status the route answered a client no longer there.
type codeWriter struct {
	http.ResponseWriter
	code int
}

func (w *codeWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// gatedImport is one import sent over a real connection, its route held at
// the client check (abandonGate) until abandon lets it go.
type gatedImport struct {
	conn    net.Conn
	gate    *abandonGate
	handled chan struct{}
	code    int // what the route answered; read once handled is closed
}

// startGatedImport sends body and returns once the route, its archive
// extracted, is asking whether its client is still there.
func startGatedImport(t *testing.T, f *importFixture, body []byte) *gatedImport {
	t.Helper()
	gi := &gatedImport{
		gate:    &abandonGate{reached: make(chan struct{}), release: make(chan struct{})},
		handled: make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(gi.handled)
		gi.gate.Context = r.Context()
		cw := &codeWriter{ResponseWriter: w}
		f.router.ServeHTTP(cw, r.WithContext(gi.gate))
		gi.code = cw.code
	}))
	// A test that stops early must not leave the route held: srv.Close waits
	// for it.
	t.Cleanup(func() {
		gi.gate.open()
		if gi.conn != nil {
			gi.conn.Close()
		}
		srv.Close()
	})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	gi.conn = conn
	fmt.Fprintf(conn, "POST /api/import HTTP/1.1\r\nHost: moombox\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := conn.Write(body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gi.gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the route never checked for its client")
	}
	return gi
}

// abandon hangs the client up — the tab closed while the panel read
// "Importing…" — and lets the route go on once the server has seen it gone.
// It returns what the route answered.
func (gi *gatedImport) abandon(t *testing.T) int {
	t.Helper()
	gi.conn.Close()
	select {
	case <-gi.gate.Context.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the server never saw the client go")
	}
	gi.gate.open()
	select {
	case <-gi.handled:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never returned")
	}
	return gi.code
}

// assertFileHolds fails unless path holds exactly want.
func assertFileHolds(t *testing.T, path string, want []byte, what string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: %s holds %q, want %q", what, path, got, want)
	}
}

// abandonVideo and abandonChat are the archive every case here imports.
var (
	abandonVideo = []byte("fake-video-bytes")
	abandonChat  = []byte(`{"messages":[]}`)
)

func abandonArchive(t *testing.T) []byte {
	return makeImportZip(t, map[string][]byte{
		"Some stream [dQw4w9WgXcQ].mp4":       abandonVideo,
		"Some stream [dQw4w9WgXcQ].chat.json": abandonChat,
	})
}

// TestImportAbandonedByItsClientCreatesNothing: a client that hung up after
// the whole body went out — the dashboard's Cancel at "Uploading... 100%"
// while the server was extracting — was told "Upload cancelled", and the
// server went on to create the job, so the retry met "job already exists".
// The route checks for a client gone before the insert now: no job, and the
// retry imports.
//
// Nothing is in place before the insert, so the abandoned import leaves no
// temporary .partial behind either: the deferred cleanup takes those back.
//
// Mutant: `&& false` on the req.Context().Err() check (the job is created and
// the retry meets 409); `false &&` (the route never asks, and every case here
// says so); the deferred temp cleanup's os.Remove(f.tmp) dropped (a .partial
// is left in imports/).
func TestImportAbandonedByItsClientCreatesNothing(t *testing.T) {
	f := newImportFixture(t)
	body := abandonArchive(t)

	if code := startGatedImport(t, f, body).abandon(t); code != 499 {
		t.Errorf("the abandoned import answered %d, want 499", code)
	}
	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Error("an import its client abandoned still created the job")
	}
	left, err := filepath.Glob(filepath.Join(f.outputDir, "imports", "*"+importPartialExt))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the abandoned import left its temporary files: %v", left)
	}

	rec, job := importZip(t, f, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the retry: %d %s", rec.Code, rec.Body.String())
	}
	assertFileHolds(t, job.OutputFile, abandonVideo, "the retry's video")
	assertFileHolds(t, job.ChatFile, abandonChat, "the retry's chat")
}

// The abandoned import's retry, sent from the reopened tab, finishes first:
// it writes the archive's own names and is answered 201. The abandoned one
// then sees its client gone — and used to take back what it had extracted,
// which were those very names, so the retry's Finished row named a video and
// a chat that no longer existed. The same holds for two imports of one
// archive in flight at once (two tabs, the dashboard and the TUI's A Z): the
// one whose client goes must not remove what the other's row names.
//
// Mutant: os.Remove(f.dest) for each file under the req.Context().Err() check
// (the retry's video or chat is gone).
func TestImportAbandonedLeavesTheRetrysFiles(t *testing.T) {
	f := newImportFixture(t)
	body := abandonArchive(t)

	abandoned := startGatedImport(t, f, body)
	rec, job := importZip(t, f, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("precondition: the retry: %d %s", rec.Code, rec.Body.String())
	}
	if code := abandoned.abandon(t); code != 499 {
		t.Fatalf("precondition: the abandoned import answered %d, want 499", code)
	}

	if !f.db.JobExists("dQw4w9WgXcQ") {
		t.Fatal("the retry's row is gone")
	}
	assertFileHolds(t, job.OutputFile, abandonVideo, "the retry's row (answered 201) names its video")
	assertFileHolds(t, job.ChatFile, abandonChat, "the retry's row (answered 201) names its chat")
}

// A row's Delete leaves its files in imports/, exactly where a re-import of
// the same archive writes. A re-import whose client goes used to remove them
// on its way out: files that were there before it ran.
//
// Mutant: os.Remove(f.dest) for each file under the req.Context().Err() check
// (the kept video or chat is gone).
func TestImportAbandonedReimportKeepsTheFilesADeletedRowLeft(t *testing.T) {
	f := newImportFixture(t)
	body := abandonArchive(t)
	rec, job := importZip(t, f, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("precondition: the first import: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.db.DeleteJob("dQw4w9WgXcQ"); err != nil {
		t.Fatal(err)
	}

	if code := startGatedImport(t, f, body).abandon(t); code != 499 {
		t.Fatalf("precondition: the abandoned re-import answered %d, want 499", code)
	}

	assertFileHolds(t, job.OutputFile, abandonVideo, "the video kept in imports/ before the abandoned re-import")
	assertFileHolds(t, job.ChatFile, abandonChat, "the chat kept in imports/ before the abandoned re-import")
}

package routes

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// holdAtEOF passes a request body through and, once it is all read, holds the
// handler until the request's context ends — the client hanging up while the
// archive is about to be extracted, deterministically.
type holdAtEOF struct {
	io.ReadCloser
	ctx  context.Context
	once sync.Once
	read chan struct{} // closed when the whole body has been read
	gone chan bool     // whether the server saw the client go
}

func (b *holdAtEOF) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(func() {
			close(b.read)
			select {
			case <-b.ctx.Done():
				b.gone <- true
			case <-time.After(10 * time.Second):
				b.gone <- false
			}
		})
	}
	return n, err
}

// TestImportAbandonedByItsClientCreatesNothing: a client that hung up after
// the whole body went out — the dashboard's Cancel at "Uploading... 100%"
// while the server was extracting — was told "Upload cancelled", and the
// server went on to create the job, so the retry met "job already exists".
// The route checks for a client gone before the insert now, and takes back
// what it extracted.
//
// Mutant: `false &&` on the req.Context().Err() check (the job is created),
// or dropping either os.Remove under it (the video or the chat stays in
// imports/).
func TestImportAbandonedByItsClientCreatesNothing(t *testing.T) {
	f := newImportFixture(t)
	read := make(chan struct{})
	gone := make(chan bool, 1)
	handled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handled)
		r.Body = &holdAtEOF{ReadCloser: r.Body, ctx: r.Context(), read: read, gone: gone}
		f.router.ServeHTTP(w, r)
	}))
	defer srv.Close()

	body := makeImportZip(t, map[string][]byte{
		"Some stream [dQw4w9WgXcQ].mp4":       []byte("fake-video-bytes"),
		"Some stream [dQw4w9WgXcQ].chat.json": []byte(`{"messages":[]}`),
	})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/import HTTP/1.1\r\nHost: moombox\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := conn.Write(body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never read the whole body")
	}
	conn.Close() // xhr.abort()
	if !<-gone {
		t.Fatal("precondition: the server never saw the client go")
	}
	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never returned")
	}

	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Error("an import its client abandoned still created the job")
	}
	entries, _ := os.ReadDir(filepath.Join(f.outputDir, "imports"))
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("an abandoned import left %v in imports/", names)
	}
}

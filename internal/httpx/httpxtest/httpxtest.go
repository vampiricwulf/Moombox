// Package httpxtest holds Go-test-only HTTP helpers shared across packages,
// beside internal/httpx as net/http/httptest sits beside net/http. It is not
// a _test.go file so that the packages that need it can import it.
//
// NewClient serves an http.Handler to an http.Client in the same process,
// without a socket, so a test can run a real request inside a
// testing/synctest bubble. A bubble's fake clock moves only once every
// goroutine in it is blocked on something the bubble can see — a channel, a
// timer — and a goroutine reading a socket is not, so an httptest server
// keeps the clock from moving. With NewClient the handler's waits
// (time.After between chunks) and the code's own timers (an idle deadline, a
// stall timer) share the fake clock, and no pause of the test process can
// come between them.
package httpxtest

import (
	"io"
	"net/http"
)

// NewClient returns an http.Client whose every request is answered by h in
// the caller's process, the response body streamed through an io.Pipe. Each
// Write the handler makes is read by the client as it is made, as over a
// socket: a Write returns once the client has read it, so Flush has nothing
// to do. The request's context reaches both ends: a cancelled request fails
// the client's next read and the handler's next Write, and the handler's
// r.Context() is the request's. A handler that writes nothing answers 200,
// as net/http's server does.
//
// A pipe blocks on channels alone, which a synctest bubble can wait out, so
// the client works inside one; the pipe and the handler's goroutine belong to
// the bubble of the goroutine that sends the request. It works outside a
// bubble too.
func NewClient(h http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{h}}
}

type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// The handler gets the request as a server would hand it over: a body
	// even for a GET, the host and the request URI filled in.
	sreq := req.Clone(ctx)
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	if sreq.Host == "" {
		sreq.Host = req.URL.Host
	}
	sreq.RequestURI = req.URL.RequestURI()

	pr, pw := io.Pipe()
	w := &responseWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer func() {
			w.WriteHeader(http.StatusOK)
			// A handler that returned because the request was cancelled
			// must not end the body cleanly: over a socket the client's read
			// fails once its request is cancelled, whatever the server does
			// next. A plain Close here could beat the watcher below and hand
			// the client an EOF instead — a stalled download read as a
			// complete one.
			if err := ctx.Err(); err != nil {
				pw.CloseWithError(err)
			} else {
				pw.Close()
			}
		}()
		ht.h.ServeHTTP(w, sreq)
	}()
	go func() {
		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
		case <-served:
		}
	}()

	select {
	case <-w.ready:
	case <-ctx.Done():
		pr.Close()
		return nil, ctx.Err()
	}
	return &http.Response{
		Status:        http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// responseWriter is the handler's side of handlerTransport. Only the
// handler's goroutine touches it until ready is closed; RoundTrip reads
// status and sent only after that.
type responseWriter struct {
	header      http.Header
	sent        http.Header // the header as WriteHeader found it
	pw          *io.PipeWriter
	status      int
	wroteHeader bool
	ready       chan struct{}
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.sent = w.header.Clone()
	close(w.ready)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush does nothing: every Write already waits for the client to read it.
func (w *responseWriter) Flush() {}

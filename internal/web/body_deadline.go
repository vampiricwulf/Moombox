package web

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// bodyReadTimeout bounds how long a request's body may take to arrive. For
// every route but the archive import it is the whole body's budget, counted
// from the moment the headers were read; for POST /api/import it is the
// longest one read may wait for the peer.
const bodyReadTimeout = 60 * time.Second

// importPath is the one route whose body is a large upload (up to 500 MB):
// its deadline moves with each read, so a slow link that keeps sending is
// never cut off and only a stall is.
const importPath = "/api/import"

// bodyReadDeadline puts a read deadline on the connection of every request
// that carries a body, so a peer that sends its headers and then stalls
// cannot hold a socket and a goroutine for as long as it likes. The server
// has ReadHeaderTimeout but, on purpose, no ReadTimeout (it would cut video
// streaming and long handlers), and net/http's own drain of an unread body
// after the handler returns has no deadline at all.
//
// It is the server's outermost handler after outermostRecovery: ahead of the
// host gate, CSRF and auth, because the request those refuse without reading
// its body is exactly the unauthenticated case.
//
// The rules, each of which matters:
//   - A request with no body (Content-Length 0 and not chunked) gets no
//     deadline. net/http has a background read running on such a connection
//     from before the handler starts, to notice a disconnect, and a deadline
//     firing under that read cancels the request context, which would abort
//     a long handler.
//   - The deadline is cleared on the Read that returns io.EOF, and never set
//     again after it: that is when net/http starts the same background read
//     for a request with a body. net/http/1 clears the deadline itself at
//     that moment; this does not lean on it.
//   - A handler that returns without finishing the body leaves the deadline
//     armed, so net/http's drain is bounded. The server replaces it with the
//     idle deadline before it waits for the next request.
//   - A WebSocket upgrade is left alone: the socket outlives any deadline
//     set here.
func bodyReadDeadline(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			// A writer that cannot reach its connection (a test recorder):
			// nothing to bound.
			next.ServeHTTP(w, r)
			return
		}
		r.Body = &deadlineBody{
			ReadCloser: r.Body,
			rc:         rc,
			timeout:    timeout,
			perRead:    r.URL.Path == importPath,
		}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody is the request body bodyReadDeadline installs.
type deadlineBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	timeout time.Duration
	// perRead moves the deadline ahead of every Read, so it bounds one
	// read's wait for the peer rather than the whole body, however long the
	// handler spends between reads.
	perRead bool
	cleared bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	if b.perRead && !b.cleared {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.timeout))
	}
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.cleared {
		b.cleared = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

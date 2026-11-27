package cipher

import (
	"io"
	"net"
	"net/http"
	"strings"
)

// keepTestsOffTheNetwork puts the player fetch behind a transport that
// answers every host but a loopback one (the test servers) with a 404, as
// YouTube answers for a player it has pulled; TestMain calls it. A goja
// resolver that a test leaves without a compiled player for its id is asked
// for signatures too (owner ruling RV-12), and it fetched that id from
// www.youtube.com. A 404 is not retried, so such a test neither waits nor
// reaches the network. Tests that need their own fetch behaviour still swap
// playerHTTPClient for themselves.
func keepTestsOffTheNetwork() {
	playerHTTPClient = &http.Client{Transport: loopbackOnly{http.DefaultTransport}}
}

// loopbackOnly answers a request to any host that is not a loopback address
// with a 404.
type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found (cipher tests: no network)",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	}
	return l.next.RoundTrip(r)
}

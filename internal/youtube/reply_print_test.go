package youtube

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// A page result outlives its fetch and travels into the worker with the
// page's reply inside it. Printing one whole, with any verb, must not print a
// cookie: not the ones the request carried and not the ones the reply set.
//
// Mutation: hold the reply's renewals by value in cookies.YouTubeReply, or
// keep the request's Cookie header in it.
func TestPageResultsPrintNoCookieValue(t *testing.T) {
	const sentinel = "SENTINEL-c00kie-VALUE"
	u, err := url.Parse("https://www.youtube.com/watch?v=abc12345678")
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{URL: u, Header: make(http.Header)}
	req.Header.Set("Cookie", "SAPISID="+sentinel+"1; LOGIN_INFO="+sentinel+"2")
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Request: req}
	resp.Header.Add("Set-Cookie", "__Secure-1PSIDCC="+sentinel+"3; Domain=.youtube.com; Path=/; Max-Age=31536000")
	resp.Header.Add("Set-Cookie", "LOGIN_INFO=; Max-Age=0; Domain=.youtube.com")

	reply := cookies.ReplyOf(resp).From("watch page", pageReplyClient, cookies.ReplyNoVerdict)
	if reply.Empty() {
		t.Fatal("premise broken: the reply holds nothing, so printing it proves nothing")
	}
	wp := &WatchPageResult{reply: reply}
	ep := &EmbedPageResult{reply: reply}

	var out strings.Builder
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		fmt.Fprintf(&out, verb+"\n", reply)
		fmt.Fprintf(&out, verb+"\n", &reply)
		fmt.Fprintf(&out, verb+"\n", wp)
		fmt.Fprintf(&out, verb+"\n", *wp)
		fmt.Fprintf(&out, verb+"\n", ep)
		fmt.Fprintf(&out, verb+"\n", *ep)
	}
	if strings.Contains(out.String(), "SENTINEL") {
		t.Errorf("a cookie value was printed:\n%s", out.String())
	}
}

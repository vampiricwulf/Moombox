package redact

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// clientIP stands in for the operator's public address in every googlevideo
// URL below; no row may let it, or anything spelled SECRET, through.
const clientIP = "203.0.113.77"

func assertNoMediaSecret(t *testing.T, what, got string) {
	t.Helper()
	for _, s := range []string{clientIP, "SECRET"} {
		if strings.Contains(got, s) {
			t.Errorf("%s = %q keeps %s", what, got, s)
		}
	}
}

// TestMediaURL pins the googlevideo rule for both places a media URL carries
// its parameters — the query, and the /key/value path form of a live
// manifest and an HLS segment — and that only the parameters that name the
// format or its part keep their values.
//
// Mutants (run):
//   - MediaURL without its path loop: the manifest, HLS and segment rows keep
//     the client's IP and the signature.
//   - MediaURL without its query loop: the videoplayback rows keep them.
//   - mediaParamsStart starting /api/manifest/<kind>'s pairs one segment
//     early: "dash" is read as a key, every value as the next key, and the
//     manifest rows keep the event id and the signature.
//   - isGooglevideoHost answering false: the "googlevideo host, another
//     endpoint" rows keep the IP.
//   - the fragment kept: the "fragment" row keeps it.
//   - MediaURL without its isTwitchMedia branch: every Twitch row keeps its
//     session.
//   - isTwitchMedia without its /v1/ path test: the "a test server's segment
//     endpoint" row keeps it; answering false for a ttvnw.net or
//     live-video.net host: the "another layout" and "no /v1/ path" rows.
//   - twitchPath keeping every segment but the last: the "another layout"
//     row keeps the session ahead of the file.
//   - isFileExt without its length bound: the "a dot inside the session" row
//     keeps the session's tail as an extension.
//   - the Twitch branch appending the query as it came: the "another layout"
//     and usher rows keep their query values.
func TestMediaURL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"videoplayback, query form",
			"https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=SECRETEI&ip=" + clientIP + "&id=o-SECRETID&itag=140&source=youtube&requiressl=yes&mime=audio%2Fmp4&clen=12345&dur=60.0&sparams=expire%2Cei%2Cip&sig=SECRETSIG&lsparams=mh&lsig=SECRETLSIG&n=SECRETN&pot=SECRETPOT&sq=5&range=0-1023",
			"https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=<redacted>&ip=<redacted>&id=<redacted>&itag=140&source=youtube&requiressl=<redacted>&mime=audio%2Fmp4&clen=12345&dur=60.0&sparams=<redacted>&sig=<redacted>&lsparams=<redacted>&lsig=<redacted>&n=<redacted>&pot=<redacted>&sq=5&range=0-1023",
		},
		{
			"live DASH manifest, path form",
			"https://manifest.googlevideo.com/api/manifest/dash/expire/1700000000/ei/SECRETEI/ip/" + clientIP + "/id/abc.1/source/yt_live_broadcast/sparams/expire,ei,ip,id,source/sig/SECRETSIG/pot/SECRETPOT",
			"https://manifest.googlevideo.com/api/manifest/dash/expire/1700000000/ei/<redacted>/ip/<redacted>/id/<redacted>/source/yt_live_broadcast/sparams/<redacted>/sig/<redacted>/pot/<redacted>",
		},
		{
			"HLS variant playlist",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1700000000/ei/SECRETEI/ip/" + clientIP + "/id/abc.1/itag/95/source/yt_live_broadcast/sig/SECRETSIG/playlist/index.m3u8/pot/SECRETPOT",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1700000000/ei/<redacted>/ip/<redacted>/id/<redacted>/itag/95/source/yt_live_broadcast/sig/<redacted>/playlist/index.m3u8/pot/<redacted>",
		},
		{
			"HLS segment, path form",
			"https://rr5---sn-abc.googlevideo.com/videoplayback/id/abc.1/itag/95/source/yt_live_broadcast/expire/1700000000/ip/" + clientIP + "/sig/SECRETSIG/lsig/SECRETLSIG/sq/1234/file/seg.ts",
			"https://rr5---sn-abc.googlevideo.com/videoplayback/id/<redacted>/itag/95/source/yt_live_broadcast/expire/1700000000/ip/<redacted>/sig/<redacted>/lsig/<redacted>/sq/1234/file/seg.ts",
		},
		{
			"a test server's media endpoint",
			"http://127.0.0.1:40181/videoplayback?expire=1&ei=x&ip=" + clientIP + "&id=o-abc&itag=140&source=youtube&sig=AOq0SECRETSIG&pot=<redacted>",
			"http://127.0.0.1:40181/videoplayback?expire=1&ei=<redacted>&ip=<redacted>&id=<redacted>&itag=140&source=youtube&sig=<redacted>&pot=<redacted>",
		},
		{
			"googlevideo host, another endpoint",
			"https://rr1---sn-abc.googlevideo.com/initplayback?source=youtube&ip=" + clientIP,
			"https://rr1---sn-abc.googlevideo.com/initplayback?source=youtube&ip=<redacted>",
		},
		{
			"googlevideo host, another endpoint, port and case",
			"https://RR1---SN-ABC.GoogleVideo.COM:443?ip=" + clientIP,
			"https://RR1---SN-ABC.GoogleVideo.COM:443?ip=<redacted>",
		},
		{"fragment", "https://rr1---sn-abc.googlevideo.com/videoplayback?itag=1#SECRETFRAG", "https://rr1---sn-abc.googlevideo.com/videoplayback?itag=1#<redacted>"},
		{"no scheme", "/videoplayback?itag=1&ip=" + clientIP, "/videoplayback?itag=1&ip=<redacted>"},
		{
			"truncated for a log line",
			"https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=SECRETEI&ip=203.0.1",
			"https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=<redacted>&ip=<redacted>",
		},
		{
			"unparseable",
			"http://[::1%zz/videoplayback?ip=" + clientIP + "&sig=SECRETSIG&itag=1",
			"http://[::1%zz/videoplayback?ip=<redacted>&sig=<redacted>&itag=1",
		},
		{"empty value and bare key kept", "https://h/videoplayback?ip=&ratebypass&itag=1", "https://h/videoplayback?ip=&ratebypass&itag=1"},
		// Twitch: the session is the path.
		{
			"Twitch weaver variant playlist",
			"https://video-weaver.fra05.hls.ttvnw.net/v1/playlist/CsoESECRETSESSION-x_y.m3u8",
			"https://video-weaver.fra05.hls.ttvnw.net/v1/playlist/<redacted>.m3u8",
		},
		{
			"Twitch edge segment",
			"https://video-edge-c2a0d4.fra05.abs.hls.ttvnw.net/v1/segment/CuwESECRETSESSION.ts",
			"https://video-edge-c2a0d4.fra05.abs.hls.ttvnw.net/v1/segment/<redacted>.ts",
		},
		{
			"Twitch, a test server's segment endpoint",
			"http://127.0.0.1:40181/v1/segment/CuwESECRETSESSION.ts",
			"http://127.0.0.1:40181/v1/segment/<redacted>.ts",
		},
		{
			"Twitch host, another layout, query and fragment",
			"https://video-edge-1.pdx01.abs.hls.live-video.net/v2/SECRETSESSION/12.ts?dna=SECRETDNA&x#SECRETFRAG",
			"https://video-edge-1.pdx01.abs.hls.live-video.net/<redacted>/<redacted>/12.ts?dna=<redacted>&x#<redacted>",
		},
		{
			"Twitch host, no /v1/ path, case",
			"https://Video-Edge.ABS.HLS.TTVNW.NET:443/SECRETSESSION.ts",
			"https://Video-Edge.ABS.HLS.TTVNW.NET:443/<redacted>.ts",
		},
		{
			"Twitch, a dot inside the session",
			"https://video-edge-x.abs.hls.ttvnw.net/v1/segment/CuwE.SECRETSESSIONTAIL",
			"https://video-edge-x.abs.hls.ttvnw.net/v1/segment/<redacted>",
		},
		{
			"Twitch usher, live",
			"https://usher.ttvnw.net/api/channel/hls/somelogin.m3u8?sig=SECRETSIG&token=SECRETTOKEN",
			"https://usher.ttvnw.net/api/channel/hls/<redacted>.m3u8?sig=<redacted>&token=<redacted>",
		},
		{
			"Twitch usher, VOD",
			"https://usher.ttvnw.net/vod/2212345678.m3u8?sig=SECRETSIG",
			"https://usher.ttvnw.net/vod/2212345678.m3u8?sig=<redacted>",
		},
		// Not a media URL: only a PO token goes.
		{"not media, PO token", "https://h/v?itag=1&pot=SECRETPOT&ip=x", "https://h/v?itag=1&pot=<redacted>&ip=x"},
		{"not media, a watch page", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10"},
		{"not media, videoplayback deeper in the path", "https://h/x/videoplayback?a=b", "https://h/x/videoplayback?a=b"},
		{"not media, a host named like one", "https://googlevideo.com.example/x?ip=1", "https://googlevideo.com.example/x?ip=1"},
		{"not media, Twitch GQL", "https://gql.twitch.tv/gql?a=b", "https://gql.twitch.tv/gql?a=b"},
		{"not media, a Twitch thumbnail", "https://static-cdn.jtvnw.net/previews-ttv/live_user_x-1280x720.jpg", "https://static-cdn.jtvnw.net/previews-ttv/live_user_x-1280x720.jpg"},
		{"not media, a Twitch host named like one", "https://ttvnw.net.example/v2/x?a=b", "https://ttvnw.net.example/v2/x?a=b"},
		{"not media, /v1/segment deeper in the path", "https://h/x/v1/segment/a.ts?a=b", "https://h/x/v1/segment/a.ts?a=b"},
		{"not media, innertube", "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MediaURL(tc.in)
			if got != tc.want {
				t.Fatalf("MediaURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if !strings.HasPrefix(tc.name, "not media") {
				assertNoMediaSecret(t, "MediaURL", got)
			}
			if again := MediaURL(got); again != got {
				t.Fatalf("a second MediaURL changed %q to %q", got, again)
			}
		})
	}
}

// TestMediaText pins the free-text rule: every URL in an already flattened
// message gets MediaURL, wherever it sits, and a PO token outside one goes too.
//
// Mutants (run):
//   - urlInTextRe stopping at '<' as well: the "after a token already cut"
//     row keeps the IP and the signature that follow the first <redacted>.
//   - MediaText without its PoTokenText pass: the "token outside a URL" row
//     keeps it.
func TestMediaText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"setJobError's shape, path form",
			`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/expire/1/ip/` + clientIP + `/id/abc.1/sig/SECRETSIG": dial tcp: connection refused`,
			`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/expire/1/ip/<redacted>/id/<redacted>/sig/<redacted>": dial tcp: connection refused`,
		},
		{
			"after a token already cut",
			`download: Get "https://rr3---sn-abc.googlevideo.com/videoplayback?pot=<redacted>&ip=` + clientIP + `&sig=SECRETSIG": EOF`,
			`download: Get "https://rr3---sn-abc.googlevideo.com/videoplayback?pot=<redacted>&ip=<redacted>&sig=<redacted>": EOF`,
		},
		{
			"two URLs, unquoted",
			"a https://h/videoplayback?ip=" + clientIP + " then https://h/videoplayback/ip/" + clientIP + "/itag/1 end",
			"a https://h/videoplayback?ip=<redacted> then https://h/videoplayback/ip/<redacted>/itag/1 end",
		},
		{
			"setJobError's shape, a Twitch variant playlist",
			`setup download: HLS playlist fetch failed after 10 consecutive errors: Get "https://video-weaver.fra05.hls.ttvnw.net/v1/playlist/CsoESECRETSESSION.m3u8": dial tcp: i/o timeout`,
			`setup download: HLS playlist fetch failed after 10 consecutive errors: Get "https://video-weaver.fra05.hls.ttvnw.net/v1/playlist/<redacted>.m3u8": dial tcp: i/o timeout`,
		},
		{"token outside a URL", "token pot=SECRETPOT and /pot/SECRETPOT", "token pot=<redacted> and /pot/<redacted>"},
		{"no URL at all", "no URL at all", "no URL at all"},
		{"not a media URL", `Get "https://www.youtube.com/youtubei/v1/player?prettyPrint=false": EOF`, `Get "https://www.youtube.com/youtubei/v1/player?prettyPrint=false": EOF`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MediaText(tc.in)
			if got != tc.want {
				t.Fatalf("MediaText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			assertNoMediaSecret(t, "MediaText", got)
		})
	}
}

// TestMediaError pins the error contract: a *url.Error found anywhere in the
// tree loses its media URL's credentials and its PO token, in either form;
// the chain and every other error pass through untouched.
//
// Mutants (run):
//   - MediaError returning err unchanged: every redacting row keeps its
//     secret.
//   - rewriteURLErrors not descending into Unwrap() []error: the "joined" row
//     keeps the token of the second url.Error.
//   - building the wrapper from err.Error() read AFTER the rewrite but without
//     MediaText, for a wrapper whose message was precomputed: the "wrapped"
//     rows keep their secrets.
func TestMediaError(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name    string
		err     error
		want    string
		samePtr bool
	}{
		{"nil", nil, "", true},
		{"not a url.Error", cause, "boom", true},
		{"no secret", &url.Error{Op: "Get", URL: "https://h/v?itag=1", Err: cause}, `Get "https://h/v?itag=1": boom`, true},
		{"bare, query form", &url.Error{Op: "Get", URL: "https://h/v?itag=1&pot=SECRET&x=2", Err: cause}, `Get "https://h/v?itag=1&pot=<redacted>&x=2": boom`, false},
		{"bare, path form", &url.Error{Op: "Get", URL: "https://h/index.m3u8/pot/SECRET", Err: cause}, `Get "https://h/index.m3u8/pot/<redacted>": boom`, false},
		{"bare, media URL", &url.Error{Op: "Get", URL: "https://rr3---sn-abc.googlevideo.com/videoplayback?ip=" + clientIP + "&itag=140&sig=SECRETSIG", Err: cause},
			`Get "https://rr3---sn-abc.googlevideo.com/videoplayback?ip=<redacted>&itag=140&sig=<redacted>": boom`, false},
		{"wrapped, query form", fmt.Errorf("download: %w", &url.Error{Op: "Get", URL: "https://h/v?pot=SECRET", Err: cause}), `download: Get "https://h/v?pot=<redacted>": boom`, false},
		{"wrapped, path form", fmt.Errorf("fetch HLS manifest: %w", &url.Error{Op: "Get", URL: "https://h/index.m3u8/pot/SECRET", Err: cause}), `fetch HLS manifest: Get "https://h/index.m3u8/pot/<redacted>": boom`, false},
		{"wrapped, media path form", fmt.Errorf("fetch DASH manifest: %w", &url.Error{Op: "Get", URL: "https://manifest.googlevideo.com/api/manifest/dash/ip/" + clientIP + "/sig/SECRETSIG", Err: cause}),
			`fetch DASH manifest: Get "https://manifest.googlevideo.com/api/manifest/dash/ip/<redacted>/sig/<redacted>": boom`, false},
		{"joined", errors.Join(&url.Error{Op: "Get", URL: "https://h/a", Err: cause}, &url.Error{Op: "Get", URL: "https://h/b/pot/SECRET", Err: cause}),
			"Get \"https://h/a\": boom\nGet \"https://h/b/pot/<redacted>\": boom", false},
		{"unparseable URL", &url.Error{Op: "parse", URL: "http://[::1%zz/v?pot=SECRET&a=b", Err: cause}, `parse "http://[::1%zz/v?pot=<redacted>&a=b": boom`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MediaError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("MediaError(nil) = %v", got)
				}
				return
			}
			if got.Error() != tc.want {
				t.Fatalf("Error() = %q, want %q", got.Error(), tc.want)
			}
			assertNoMediaSecret(t, "Error()", got.Error())
			if tc.samePtr && got != tc.err {
				t.Fatalf("MediaError returned a new error for %q, want it untouched", tc.name)
			}
			if errors.Is(tc.err, cause) && !errors.Is(got, cause) {
				t.Fatalf("errors.Is(cause) lost")
			}
			if again := MediaError(got); again.Error() != got.Error() {
				t.Fatalf("second MediaError changed the text: %q -> %q", got.Error(), again.Error())
			}
		})
	}
}

// TestMediaErrorKeepsTransportCause drives a real transport failure: the
// *url.Error and the *net.OpError under it must survive the redaction, since
// callers classify transport errors with errors.As.
func TestMediaErrorKeepsTransportCause(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	resp, err := (&http.Client{Transport: &http.Transport{}}).Get("http://" + addr + "/api/manifest/dash/ip/" + clientIP + "/id/abc/sig/SECRETSIG/pot/SECRETPOT")
	if err == nil {
		resp.Body.Close()
		t.Fatal("want a transport error from a refused port")
	}
	got := MediaError(fmt.Errorf("fetch DASH manifest: %w", err))
	assertNoMediaSecret(t, "Error()", got.Error())
	var ue *url.Error
	if !errors.As(got, &ue) || ue.Op != "Get" {
		t.Fatalf("errors.As(*url.Error) lost on %q", got.Error())
	}
	var oe *net.OpError
	if !errors.As(got, &oe) || oe.Op != "dial" {
		t.Fatalf("errors.As(*net.OpError) lost on %q", got.Error())
	}
}

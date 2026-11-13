package youtube

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// queryTransport answers every player call with adequateOKBody and records
// each request's URL path and query string.
type queryTransport struct {
	paths   []string
	queries []string
}

func (tr *queryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.paths = append(tr.paths, req.URL.Path)
	tr.queries = append(tr.queries, req.URL.RawQuery)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(adequateOKBody)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestInnertubeCallsAskForCondensedJSON: every /player call (the
// authenticated and probe path, the cookieless clients, web_embedded) and the
// /browse call carry prettyPrint=false after the key, as yt-dlp's _call_api
// sends it (_base.py:862-866), so YouTube answers without indentation. A
// /browse call made with no key has the parameter alone, not a dangling "&".
//
// Mutants: the parameter dropped from any one of the three player paths or
// from browse; "&prettyPrint=false" appended when there is no key.
func TestInnertubeCallsAskForCondensedJSON(t *testing.T) {
	tr := &queryTransport{}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := newRetryTestAPI()
	p.SetAPIKey("test-key")
	ctx := context.Background()
	if _, err := p.fetchWithClient(ctx, "test1234567", constants.WebClient, DefaultYtcfg(), 0); err != nil {
		t.Fatalf("fetchWithClient: %v", err)
	}
	if _, err := p.fetchWithCookielessClient(ctx, "test1234567", "vd", constants.AndroidVRClient); err != nil {
		t.Fatalf("fetchWithCookielessClient: %v", err)
	}
	if _, err := p.fetchWithEmbedded(ctx, "test1234567", DefaultYtcfg(), 0, false); err != nil {
		t.Fatalf("fetchWithEmbedded: %v", err)
	}
	if len(tr.queries) != 3 {
		t.Fatalf("recorded %d player calls, want 3", len(tr.queries))
	}
	for i, q := range tr.queries {
		if tr.paths[i] != "/youtubei/v1/player" || q != "key=test-key&prettyPrint=false" {
			t.Errorf("player call %d went to %s?%s, want /youtubei/v1/player?key=test-key&prettyPrint=false", i, tr.paths[i], q)
		}
	}

	apiClient = orig // the browse rows go to their own test server
	for _, tc := range []struct{ key, want string }{
		{key: "test-key", want: "key=test-key&prettyPrint=false"},
		{key: "", want: "prettyPrint=false"},
	} {
		var got string
		s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.RawQuery
			_, _ = w.Write([]byte(`{}`))
		})
		// SetAPIKey ignores "", so the keyless row sets the field itself.
		s.PlayerAPI.apiKeyMu.Lock()
		s.PlayerAPI.apiKey = tc.key
		s.PlayerAPI.apiKeyMu.Unlock()
		if _, err := s.browsePost(ctx, map[string]any{"browseId": "UCbrowsetest0000000000ab"}, "vd"); err != nil {
			t.Fatalf("browsePost: %v", err)
		}
		if got != tc.want {
			t.Errorf("browse query with key %q = %q, want %q", tc.key, got, tc.want)
		}
	}
}

package youtube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// bodyTransport answers every player call with adequateOKBody and records
// each request's decoded JSON body and headers.
type bodyTransport struct {
	bodies  []map[string]any
	headers []http.Header
}

func (tr *bodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body map[string]any
	raw, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(raw, &body)
	tr.bodies = append(tr.bodies, body)
	tr.headers = append(tr.headers, req.Header.Clone())
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(adequateOKBody)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// The authenticated TV probe identifies itself as the Samsung Tizen set
// (yt-dlp's tv_samsung): the device fields in the Innertube client context
// and the matching User-Agent. The bare TVHTML5 identity is answered "The
// page needs to be reloaded" with no formats for a signed-in session, on
// every video, members-only and age-gated ones included — the probe that
// polls a members-only waiting room, and the cascade's status authority,
// both ran on it.
//
// Mutants: TVDowngradedClient put back; a device field dropped from the
// context (YouTube keys on them, not the UA alone).
func TestAuthenticatedTVProbeIsTheSamsungIdentity(t *testing.T) {
	tr := &bodyTransport{}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	if _, err := newRetryTestAPI().ProbeVideoStatusAuthenticated(context.Background(), "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
	}
	if len(tr.bodies) != 1 {
		t.Fatalf("recorded %d player calls, want 1", len(tr.bodies))
	}
	if got := tr.headers[0].Get("X-YouTube-Client-Name"); got != "7" {
		t.Errorf("X-YouTube-Client-Name = %q, want 7 (TVHTML5)", got)
	}
	assertSamsungIdentity(t, "probe", tr.bodies[0], tr.headers[0])
}

// The cascades' TV call is the Samsung identity too, on both paths: it is
// the status authority of the authenticated cascade and the lead client of
// the public one, and on the bare identity every full extraction got "The
// page needs to be reloaded" with no formats from it — the failure the
// Samsung identity was adopted to end. The probe test above runs neither
// cascade.
//
// Mutants: TVDowngradedClient put back in GetVideoInfoAuthenticated's TV
// call ("authenticated" fails), or in GetVideoInfoPublic's ("public" fails).
func TestBothCascadesCallTheSamsungTV(t *testing.T) {
	for _, path := range []string{"authenticated", "public"} {
		t.Run(path, func(t *testing.T) {
			stubWatchPage(t)
			tr := &bodyTransport{}
			orig := apiClient
			apiClient = &http.Client{Transport: tr}
			t.Cleanup(func() { apiClient = orig })

			p := newRetryTestAPI()
			var err error
			if path == "authenticated" {
				_, err = p.GetVideoInfoAuthenticated(context.Background(), "test1234567")
			} else {
				_, err = p.GetVideoInfoPublic(context.Background(), "test1234567")
			}
			if err != nil {
				t.Fatalf("GetVideoInfo (%s): %v", path, err)
			}
			tvCalls := 0
			for i, h := range tr.headers {
				if h.Get("X-YouTube-Client-Name") != "7" {
					continue
				}
				tvCalls++
				assertSamsungIdentity(t, path+" cascade TV call", tr.bodies[i], h)
			}
			if tvCalls == 0 {
				t.Fatalf("the %s cascade made no TV call", path)
			}
		})
	}
}

// assertSamsungIdentity checks one recorded TVHTML5 player call for the
// Samsung Tizen identity: the device fields in the Innertube client context
// and the matching User-Agent.
func assertSamsungIdentity(t *testing.T, label string, body map[string]any, h http.Header) {
	t.Helper()
	ctxMap, _ := body["context"].(map[string]any)
	client, _ := ctxMap["client"].(map[string]any)
	for k, want := range map[string]string{
		"clientName": "TVHTML5", "clientVersion": "5.20260707",
		"deviceMake": "Samsung", "deviceModel": "UKS9800", "osName": "Tizen", "osVersion": "2.4.0",
	} {
		if got, _ := client[k].(string); got != want {
			t.Errorf("%s: context.client.%s = %q, want %q", label, k, got, want)
		}
	}
	if got := h.Get("User-Agent"); got != constants.UserAgents.TVSamsung || !strings.Contains(got, "Tizen") {
		t.Errorf("%s: User-Agent = %q, want the Samsung Tizen UA", label, got)
	}
}

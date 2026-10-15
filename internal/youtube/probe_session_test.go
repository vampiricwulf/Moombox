package youtube

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// headerTransport answers every player call with adequateOKBody and records
// the request headers.
type headerTransport struct{ headers []http.Header }

func (tr *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.headers = append(tr.headers, req.Header.Clone())
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(adequateOKBody)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// The probe-only calls carry the signed-in session the last watch page
// stated — X-Goog-AuthUser and X-Goog-PageId — as every call of a full
// extraction does. A membership held on a brand account exists only under
// that delegated identity: without the headers the probe is judged as the
// primary account and refused, so the members-only waiting room's status
// probe, the schedule probe and the date probe would all read a stream the
// user can archive as one they cannot. A later page that names no session
// (the public path's cookieless page) does not clear it.
//
// Mutants: probeYtcfg building a bare DefaultYtcfg (no headers);
// captureVisitorData overwriting the session with an empty one.
func TestProbesCarryTheWatchPagesSession(t *testing.T) {
	tr := &headerTransport{}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := newRetryTestAPI()
	idx := 2
	p.captureVisitorData(&YtcfgData{VisitorData: "vd", SessionIndex: &idx, DelegatedSessionID: "brand-123"})
	p.captureVisitorData(&YtcfgData{VisitorData: "vd-anon"}) // a cookieless page: no session, nothing cleared

	ctx := context.Background()
	if _, err := p.ProbeVideoStatusAuthenticated(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
	}
	if _, err := p.ProbeVideoSchedule(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoSchedule: %v", err)
	}
	if _, _, err := p.ProbeVideoDate(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
	if len(tr.headers) != 3 {
		t.Fatalf("recorded %d player calls, want 3", len(tr.headers))
	}
	for i, h := range tr.headers {
		if got := h.Get("X-Goog-AuthUser"); got != "2" {
			t.Errorf("call %d: X-Goog-AuthUser = %q, want 2", i, got)
		}
		if got := h.Get("X-Goog-PageId"); got != "brand-123" {
			t.Errorf("call %d: X-Goog-PageId = %q, want brand-123", i, got)
		}
	}
}

// A page that states a new session replaces the old one whole: an operator
// who moved from a brand account to the primary must not keep sending the
// brand's page id.
func TestANewSessionReplacesTheOldOne(t *testing.T) {
	p := newRetryTestAPI()
	idx := 1
	p.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-123"})
	zero := 0
	p.captureVisitorData(&YtcfgData{SessionIndex: &zero})
	got := p.probeYtcfg("")
	if got.DelegatedSessionID != "" || got.SessionIndex == nil || *got.SessionIndex != 0 {
		t.Errorf("probe ytcfg = index %v, delegated %q; want index 0 and no delegation", got.SessionIndex, got.DelegatedSessionID)
	}
}

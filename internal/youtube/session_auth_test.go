package youtube

import "testing"

// TestWatchPageSessionAuth pins the watch-page login detector. The markers
// are the ytcfg booleans YouTube stamps into every watch page; absence means
// the response came back logged out, NOT "we don't know" — the page was
// fetched and answered, so the third state (unknown) belongs to the caller
// that never got a page at all.
func TestWatchPageSessionAuth(t *testing.T) {
	tests := []struct {
		name string
		html string
		want SessionAuthState
	}{
		{
			name: "ytcfg LOGGED_IN marker",
			html: `<script>ytcfg.set({"LOGGED_IN":true,"VISITOR_DATA":"x"});</script>`,
			want: SessionAuthLoggedIn,
		},
		{
			name: "camelCase isLoggedIn marker",
			html: `<script>window.x = {"isLoggedIn":true};</script>`,
			want: SessionAuthLoggedIn,
		},
		{
			name: "explicit LOGGED_IN false",
			html: `<script>ytcfg.set({"LOGGED_IN":false});</script>`,
			want: SessionAuthLoggedOut,
		},
		{
			name: "no marker at all",
			html: `<html><body>nothing here</body></html>`,
			want: SessionAuthLoggedOut,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchPageSessionAuth(tt.html); got != tt.want {
				t.Errorf("watchPageSessionAuth = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWithAttestationPropagatesSessionAuth is the whole point of the field:
// the login state observed on the watch page must survive onto the VideoInfo
// the worker classifies, so a members-only failure can say whether the
// request was even signed in.
//
// The "watch page fetch failed" case is the one that must NOT collapse into
// "logged out": GetVideoInfoAuthenticated substitutes a synthetic
// WatchPageResult on fetch failure, and reporting that as a dead session
// would send the operator chasing cookies that may be perfectly fine.
func TestWithAttestationPropagatesSessionAuth(t *testing.T) {
	tests := []struct {
		name string
		wp   *WatchPageResult
		want SessionAuthState
	}{
		{
			name: "logged in",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg(), SessionAuth: SessionAuthLoggedIn},
			want: SessionAuthLoggedIn,
		},
		{
			name: "logged out",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg(), SessionAuth: SessionAuthLoggedOut},
			want: SessionAuthLoggedOut,
		},
		{
			name: "watch page fetch failed (synthetic result) stays unknown",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg()},
			want: SessionAuthUnknown,
		},
		{
			name: "no watch page at all stays unknown",
			wp:   nil,
			want: SessionAuthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := withAttestation(&VideoInfo{}, tt.wp, "vid123")
			if info.SessionAuth != tt.want {
				t.Errorf("SessionAuth = %q, want %q", info.SessionAuth, tt.want)
			}
		})
	}
}

// TestWithAttestationSessionAuthDrivesBinding guards the refactor that
// replaced WatchPageResult.IsLoggedIn with SessionAuth: the GVS content
// binding must still pick the datasyncID branch for a signed-in session and
// the visitorData branch otherwise (including "unknown", which is what the
// old failed-fetch path passed as false).
func TestWithAttestationSessionAuthDrivesBinding(t *testing.T) {
	cfg := &YtcfgData{DataSyncID: "ds", VisitorData: "vd"}

	in := withAttestation(&VideoInfo{}, &WatchPageResult{Ytcfg: cfg, SessionAuth: SessionAuthLoggedIn}, "vid123")
	if in.GvsBindingKind != BindingDataSyncID {
		t.Errorf("logged in: binding kind = %q, want %q", in.GvsBindingKind, BindingDataSyncID)
	}

	for _, state := range []SessionAuthState{SessionAuthLoggedOut, SessionAuthUnknown} {
		out := withAttestation(&VideoInfo{}, &WatchPageResult{Ytcfg: cfg, SessionAuth: state}, "vid123")
		if out.GvsBindingKind != BindingVisitorData {
			t.Errorf("%q: binding kind = %q, want %q", state, out.GvsBindingKind, BindingVisitorData)
		}
	}
}

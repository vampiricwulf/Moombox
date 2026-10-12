package youtube

import (
	"context"
	"testing"
)

// isPremiereResponse is the premiere signal classifyStream decides on (the
// display-only IsPremiere widens it; see TestPlayingPremiereIsRecognised). A
// live broadcast (isLiveContent) is never a premiere.
func TestIsPremiereResponse(t *testing.T) {
	lbd := map[string]any{"liveBroadcastDetails": map[string]any{"startTimestamp": "2026-10-12T18:00:00+00:00"}}
	for _, tc := range []struct {
		name string
		vd   map[string]any
		ps   map[string]any
		mf   map[string]any
		want bool
	}{
		{"upcoming premiere", map[string]any{"isUpcoming": true}, nil, lbd, true},
		{"premiere by reason", map[string]any{}, map[string]any{"reason": "Premieres in 3 hours"}, lbd, true},
		{"upcoming live broadcast", map[string]any{"isUpcoming": true, "isLiveContent": true}, nil, lbd, false},
		{"no broadcast details", map[string]any{"isUpcoming": true}, nil, nil, false},
	} {
		if got := isPremiereResponse(tc.vd, tc.ps, tc.mf); got != tc.want {
			t.Errorf("%s: isPremiereResponse = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A non-web client's response may lack the microformat the premiere signal
// and the upload date both come from; the watch page's parse supplies them,
// and never clears what the client already had.
//
// Mutants: dropping the IsPremiere or the UploadDate merge from
// mergeWatchPageMetadata.
func TestWatchPageMergeCarriesIsPremiereAndUploadDate(t *testing.T) {
	target := &VideoInfo{}
	mergeWatchPageMetadata(target, &VideoInfo{IsPremiere: true, UploadDate: "2026-10-01"})
	if !target.IsPremiere {
		t.Error("the watch page's premiere flag was not merged")
	}
	if target.UploadDate != "2026-10-01" {
		t.Errorf("UploadDate = %q, want the watch page's", target.UploadDate)
	}
	target = &VideoInfo{IsPremiere: true, UploadDate: "2026-09-30"}
	mergeWatchPageMetadata(target, &VideoInfo{})
	if !target.IsPremiere {
		t.Error("a watch page without the signal cleared the client's premiere flag")
	}
	if target.UploadDate != "2026-09-30" {
		t.Errorf("UploadDate = %q, want the client's own kept", target.UploadDate)
	}
}

// While a premiere plays its response no longer says upcoming and carries no
// "premiere" reason; the display flag still recognises it from broadcast
// details on a video that is EXPLICITLY not live content. A live broadcast,
// which is live content, never reads as a premiere — nor does a response that
// does not say either way.
//
// Mutants: IsPremiere set from isPremiereResponse alone (the playing premiere
// reads as a plain live stream); the display arm reading an absent
// isLiveContent as false (every live broadcast a client sends without it reads
// as a premiere).
func TestPlayingPremiereIsRecognised(t *testing.T) {
	for _, tc := range []struct {
		name          string
		isLiveContent string // the videoDetails field, or "" for none
		want          bool
	}{
		{"premiere playing", `, "isLiveContent": false`, true},
		{"live broadcast", `, "isLiveContent": true`, false},
		{"isLiveContent absent", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{
				"playabilityStatus": {"status": "OK"},
				"videoDetails": {"videoId": "vid", "isLive": true` + tc.isLiveContent + `},
				"microformat": {"playerMicroformatRenderer": {"liveBroadcastDetails": {"isLiveNow": true, "startTimestamp": "2026-10-12T18:00:00+00:00"}}}
			}`
			info, err := NewPlayerAPI(nil, noopLogger{}).parsePlayerResponse(context.Background(), decodePlayerJSON(t, body), "", nil, "vid")
			if err != nil {
				t.Fatalf("parsePlayerResponse: %v", err)
			}
			if !info.IsLive {
				t.Fatalf("IsLive = false; this test needs a playing stream (status %s)", info.StreamStatus)
			}
			if info.IsPremiere != tc.want {
				t.Errorf("IsPremiere = %v, want %v", info.IsPremiere, tc.want)
			}
		})
	}
}

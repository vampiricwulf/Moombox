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
// needs; the watch page's parse supplies it.
//
// Mutant: dropping the IsPremiere merge from mergeWatchPageMetadata.
func TestWatchPageMergeCarriesIsPremiere(t *testing.T) {
	target := &VideoInfo{}
	mergeWatchPageMetadata(target, &VideoInfo{IsPremiere: true})
	if !target.IsPremiere {
		t.Error("the watch page's premiere flag was not merged")
	}
	target = &VideoInfo{IsPremiere: true}
	mergeWatchPageMetadata(target, &VideoInfo{})
	if !target.IsPremiere {
		t.Error("a watch page without the signal cleared the client's premiere flag")
	}
}

// While a premiere plays its response no longer says upcoming and carries no
// "premiere" reason; the display flag still recognises it from broadcast
// details on a video that is explicitly not live content. A live broadcast,
// which is live content, never reads as a premiere.
//
// Mutant: IsPremiere set from isPremiereResponse alone (the playing premiere
// reads as a plain live stream).
func TestPlayingPremiereIsRecognised(t *testing.T) {
	for _, tc := range []struct {
		name          string
		isLiveContent bool
		want          bool
	}{
		{"premiere playing", false, true},
		{"live broadcast", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{
                "playabilityStatus": {"status": "OK"},
                "videoDetails": {"videoId": "vid", "isLive": true, "isLiveContent": ` + map[bool]string{true: "true", false: "false"}[tc.isLiveContent] + `},
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

package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// stubWatchPageSaying answers the watch-page fetch with a page YouTube served
// to a session it read as state, carrying playerResponse when there is one.
func stubWatchPageSaying(t *testing.T, state SessionAuthState, playerResponse string) {
	t.Helper()
	var pr map[string]any
	if playerResponse != "" {
		if err := json.Unmarshal([]byte(playerResponse), &pr); err != nil {
			t.Fatalf("watch-page player response: %v", err)
		}
	}
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		return &WatchPageResult{Ytcfg: DefaultYtcfg(), PlayerResponse: pr, SessionAuth: state}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// The worker parks a private verdict at COOKIES? only when the session read
// signed out although the jar's cookies were sent. So both facts have to reach
// the VideoInfo whichever client's answer the authenticated cascade returns,
// and the public cascade must never claim to have sent any: public pages read
// signed out too, and a signed-out install would park every private video.
//
// Mutants: withAttestation not copying the credentialed fact, or SessionAuth
// (every authenticated row); the fact set on the public path (the public row);
// a return that skips finishExtraction (that route's row).
func TestEveryCascadeReturnSaysWhetherTheSessionWasCredentialed(t *testing.T) {
	private := `{"playabilityStatus":` + privateVideoWeb + `}`
	adequate := srcBody(true, "adequate", "", "")
	for _, tc := range []struct {
		name      string
		page      string
		responses map[string]string
		verdict   PlayabilityError
	}{
		{name: "TV's answer", responses: map[string]string{"7": adequate}, verdict: PlayabilityOK},
		{name: "the watch page's verdict", page: private, responses: map[string]string{"7": private, "62": private}, verdict: PlayabilityPrivate},
		{name: "web_creator's verdict", responses: map[string]string{"7": private, "62": private}, verdict: PlayabilityPrivate},
		{name: "a cookieless fallback's answer", responses: map[string]string{"7": private, "62": private, "28": adequate}, verdict: PlayabilityOK},
		{name: "web_embedded's age-gate answer", responses: map[string]string{"7": ageRestrictedBody, "56": adequate}, verdict: PlayabilityOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubWatchPageSaying(t, SessionAuthLoggedOut, tc.page)
			useManifestSourceTransport(t, tc.responses)
			info, err := preRollTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("GetVideoInfoAuthenticated: %v", err)
			}
			if info.PlayabilityError != tc.verdict {
				t.Errorf("verdict = %q, want %q", info.PlayabilityError, tc.verdict)
			}
			if info.SessionAuth != SessionAuthLoggedOut || !info.SessionCredentialed {
				t.Errorf("SessionAuth = %q, credentialed %v; want logged_out and credentialed", info.SessionAuth, info.SessionCredentialed)
			}
		})
	}

	// No page, so no word on the session: unknown, which is not signed out.
	t.Run("no watch page", func(t *testing.T) {
		orig := fetchWatchPage
		fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
			return nil, errors.New("watch page: HTTP 503")
		}
		t.Cleanup(func() { fetchWatchPage = orig })
		useManifestSourceTransport(t, map[string]string{"7": private, "62": private})
		info, err := preRollTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("GetVideoInfoAuthenticated: %v", err)
		}
		if info.PlayabilityError != PlayabilityPrivate || info.SessionAuth != SessionAuthUnknown {
			t.Errorf("verdict %q, SessionAuth %q; want private and unknown", info.PlayabilityError, info.SessionAuth)
		}
	})

	t.Run("the public cascade", func(t *testing.T) {
		stubWatchPageSaying(t, SessionAuthLoggedOut, private)
		useManifestSourceTransport(t, map[string]string{"7": private})
		info, err := preRollTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("GetVideoInfoPublic: %v", err)
		}
		if info.PlayabilityError != PlayabilityPrivate || info.SessionAuth != SessionAuthLoggedOut {
			t.Errorf("verdict %q, SessionAuth %q; want private and logged_out", info.PlayabilityError, info.SessionAuth)
		}
		if info.SessionCredentialed {
			t.Error("the public cascade says it sent the jar's cookies")
		}
	})
}

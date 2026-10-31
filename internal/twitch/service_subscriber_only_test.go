package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// G52, owner decision D20, offline: Usher's subscriber-only 403 split by whom
// it refused, through both Service methods that fetch a playlist.
//
// The token documents are synthetic key/value pairs with an empty signature,
// as service_hls_playback_token_test.go's are, and the refusal body is the
// shape isRestrictedEntitlementBody reads (yt-dlp twitch.py's error_info).

// subscriberOnlyBody is Usher's refusal of subscriber-only content.
const subscriberOnlyBody = `[{"error":"Manifest is restricted","error_code":"vod_manifest_restricted","type":"error","url":"https://usher.test.invalid"}]`

// installSubscriberOnlyStub answers both playback-token mutations with
// tokenValue and refuses every Usher playlist with subscriberOnlyBody,
// restored in t.Cleanup. onGQL runs between the token reply being composed
// and returned — the window a jar reload falls into for a method that reads
// its auth token twice. Not parallel: twitchHTTPClient is package-wide.
func installSubscriberOnlyStub(t *testing.T, tokenValue string, onGQL func()) {
	t.Helper()
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: hlsRoundTripper(func(req *http.Request) (*http.Response, error) {
		reply := func(status int, body []byte) *http.Response {
			h := make(http.Header)
			h.Set("Content-Type", "application/json")
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
		}
		switch reqURL := req.URL.String(); {
		case strings.HasPrefix(reqURL, constants.TwitchURLs.GQL):
			sent, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			key := "streamPlaybackAccessToken"
			if strings.Contains(string(sent), "videoPlaybackAccessToken") {
				key = "videoPlaybackAccessToken"
			}
			body, err := json.Marshal(map[string]any{"data": map[string]any{
				key: map[string]any{"value": tokenValue, "signature": ""},
			}})
			if err != nil {
				return nil, err
			}
			if onGQL != nil {
				onGQL()
			}
			return reply(http.StatusOK, body), nil
		case strings.HasPrefix(reqURL, constants.TwitchURLs.UsherLive), strings.HasPrefix(reqURL, constants.TwitchURLs.UsherVOD):
			return reply(http.StatusForbidden, []byte(subscriberOnlyBody)), nil
		default:
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
	})}
}

// playlistFetchers are the two Service methods that fetch an Usher playlist,
// each reduced to its error.
var playlistFetchers = []struct {
	name  string
	fetch func(*Service) error
}{
	{"live", func(svc *Service) error {
		_, _, err := svc.GetHLSMasterPlaylist(context.Background(), "somechannel")
		return err
	}},
	{"vod", func(svc *Service) error {
		_, err := svc.GetVodHLSPlaylist(context.Background(), "123456789")
		return err
	}},
}

// TestSubscriberOnlyRefusalIsSplitByWhomItRefused is the wave's second offline
// test. A refusal of a session the playback token names as signed in becomes
// ErrSubscriberOnlyNotEntitled — yt-dlp's "your account does not have access"
// — and NOT ErrSubscriberOnly, which would park it in COOKIES? and fire the
// automatic refresh; every other refusal stays ErrSubscriberOnly, the login
// prompt. Both keep Usher's "hls playlist http 403" head.
//
// Mutants: the subscriberOnlyRefusal call dropped from GetHLSMasterPlaylist
// (the live signed-in row) or from GetVodHLSPlaylist (the vod one); the
// authToken guard dropped (the cookieless rows); the signed-in check dropped
// (the anonymous rows); ErrSubscriberOnlyNotEntitled made a wrap of
// ErrSubscriberOnly (every signed-in row's second check).
//
// Not a mutant this can see: dropping `!conclusive ||` from the predicate is
// EQUIVALENT, because PlaybackTokenSession never reports signedIn on an
// inconclusive document. The unreadable rows pin the outcome anyway.
func TestSubscriberOnlyRefusalIsSplitByWhomItRefused(t *testing.T) {
	tokenRow := row(".twitch.tv", "auth-token", "test-token-aaaa")
	for _, tc := range []struct {
		name            string
		rows            []string
		tokenValue      string
		wantNotEntitled bool
	}{
		{"signed in, not subscribed", []string{tokenRow}, `{"user_id":12345678,"subscriber":false}`, true},
		{"credentials sent, token minted for nobody", []string{tokenRow}, `{"user_id":null}`, false},
		{"credentials sent, document unreadable", []string{tokenRow}, `{"userId":12345678}`, false},
		{"cookieless install", nil, `{"user_id":12345678}`, false},
	} {
		for _, f := range playlistFetchers {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				installSubscriberOnlyStub(t, tc.tokenValue, nil)
				svc := NewService(hlsJar(t, filepath.Join(t.TempDir(), "cookies.txt"), tc.rows...), &hlsWarnRecorder{})

				err := f.fetch(svc)
				if got := errors.Is(err, ErrSubscriberOnlyNotEntitled); got != tc.wantNotEntitled {
					t.Errorf("errors.Is(err, ErrSubscriberOnlyNotEntitled) = %v, want %v (err = %v)", got, tc.wantNotEntitled, err)
				}
				if got := errors.Is(err, ErrSubscriberOnly); got == tc.wantNotEntitled {
					t.Errorf("errors.Is(err, ErrSubscriberOnly) = %v, want %v — the two refusals must never both match (err = %v)",
						got, !tc.wantNotEntitled, err)
				}
				if err == nil || !strings.HasPrefix(err.Error(), "hls playlist http 403: ") {
					t.Errorf("err = %v, want Usher's \"hls playlist http 403: \" head", err)
				}
				if tc.wantNotEntitled && !strings.Contains(err.Error(), "your account does not have access to this subscriber-only content") {
					t.Errorf("err = %q, want yt-dlp's wording", err)
				}
			})
		}
	}
}

// TestVodSubscriberOnlyVerdictReadsTheAuthTokenOnce: GetVodHLSPlaylist now
// reads the auth-token once, as GetHLSMasterPlaylist does, and the value that
// went out with the access-token request is the one the verdict uses. A
// reload between the request and the verdict is driven both ways, with a
// signed-in token document (the only one the split turns on):
//
//   - the jar GAINS a token mid-call: nothing was sent, so the refusal is the
//     login-required one. Read twice, the verdict sees a token that was never
//     sent and tells a signed-out request it is signed in without access.
//   - the jar LOSES its token mid-call: one was sent, and Twitch named the
//     session, so it is the account's refusal. Read twice, the second read sees
//     "" and sends a healthy login back to COOKIES? and a browser refresh.
//
// Mutant: GetVodHLSPlaylist calling s.Auth.GetAuthToken() again for the
// verdict — either row fails.
func TestVodSubscriberOnlyVerdictReadsTheAuthTokenOnce(t *testing.T) {
	tokenRow := row(".twitch.tv", "auth-token", "test-token-aaaa")
	for _, tc := range []struct {
		name            string
		before, after   []string
		wantNotEntitled bool
	}{
		{"the jar gains a token mid-call", nil, []string{tokenRow}, false},
		{"the jar loses its token mid-call", []string{tokenRow}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cookies.txt")
			jar := hlsJar(t, path, tc.before...)
			installSubscriberOnlyStub(t, `{"user_id":12345678}`, func() {
				hlsJarFile(t, path, tc.after...)
				if err := jar.Reload(); err != nil {
					t.Errorf("reload jar: %v", err)
				}
			})

			_, err := NewService(jar, &hlsWarnRecorder{}).GetVodHLSPlaylist(context.Background(), "123456789")
			if got := errors.Is(err, ErrSubscriberOnlyNotEntitled); got != tc.wantNotEntitled {
				t.Errorf("errors.Is(err, ErrSubscriberOnlyNotEntitled) = %v, want %v — the request and the verdict "+
					"disagreed about what was sent (err = %v)", got, tc.wantNotEntitled, err)
			}
		})
	}
}

// TestSubscriberOnlyRefusalLeavesEverythingElseAlone: the predicate re-labels
// one error and passes every other through as the same value — a nil, a
// plain Usher failure, and a refusal already re-labelled.
//
// Mutant: the type test replaced by errors.Is on either sentinel, the
// re-label then building a fresh error — the already re-labelled refusal
// comes back as a new value.
func TestSubscriberOnlyRefusalLeavesEverythingElseAlone(t *testing.T) {
	plain := errors.New("hls playlist http 404: offline")
	relabelled := &subscriberOnlyError{status: http.StatusForbidden, notEntitled: true}
	for _, err := range []error{nil, plain, relabelled} {
		if got := subscriberOnlyRefusal("test-token-aaaa", `{"user_id":12345678}`, err); got != err {
			t.Errorf("subscriberOnlyRefusal(%v) = %v, want the same error back", err, got)
		}
	}
}

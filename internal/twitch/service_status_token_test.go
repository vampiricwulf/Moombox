package twitch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// G21, owner decision D19, offline: a stream-status batch whose auth-token
// Twitch refuses is sent once more without it, and the refusal is reported.
//
// Every test here drives the real Service over the package-level HTTP client
// (installStatusStub), so what is pinned is the request sequence Twitch would
// see, not a restatement of the condition. No fixture is a credential: the
// token is the package's usual obviously-fake string, and the refusal body is
// streamlink's own fixture for a dead token (tests/plugins/test_twitch.py).

const statusTestToken = "test-token-aaaa"

// statusRefusalBody is what Twitch answers a GQL request carrying a dead
// token with, per streamlink's fixture.
const statusRefusalBody = `{"error":"Unauthorized","status":401,"message":"The \"Authorization\" token is invalid."}`

// statusRequest is one GQL request the stub saw: the Authorization header it
// carried ("" when none) and its body.
type statusRequest struct {
	auth string
	body string
}

// statusStub answers GQL requests from a per-request rule and records each
// one. Every other host is refused, as the package's other stubs refuse it.
type statusStub struct {
	mu   sync.Mutex
	seen []statusRequest
}

func (s *statusStub) requests() []statusRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statusRequest(nil), s.seen...)
}

// installStatusStub swaps twitchHTTPClient for one whose GQL answer is
// reply(authorization header) -> (status, body), restored in t.Cleanup. No test
// in this file may call t.Parallel: the var is shared with the whole package.
func installStatusStub(t *testing.T, reply func(auth string) (int, string)) *statusStub {
	t.Helper()
	stub := &statusStub{}
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		auth := req.Header.Get("Authorization")
		stub.mu.Lock()
		stub.seen = append(stub.seen, statusRequest{auth: auth, body: string(body)})
		stub.mu.Unlock()
		status, out := reply(auth)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader([]byte(out))), Request: req}, nil
	})}
	return stub
}

// statusSlot renders one channel's StreamMetadata + Comscore pair: "live" is a
// resolved user with a stream, "offline" one without, "missing" GQL's
// user:null.
func statusSlot(login, state string) string {
	const comscore = `{"data":{"user":{"stream":null}}}`
	switch state {
	case "live":
		return fmt.Sprintf(`{"data":{"user":{"id":"1","displayName":%q,"login":%q,"profileImageURL":"",`+
			`"stream":{"id":"s-%s","title":"a title","type":"live","viewersCount":1,"createdAt":"2026-10-31T00:00:00Z","game":null}}}},`,
			login, login, login) + comscore
	case "offline":
		return fmt.Sprintf(`{"data":{"user":{"id":"2","displayName":%q,"login":%q,"profileImageURL":"","stream":null}}},`, login, login) + comscore
	default:
		return `{"data":{"user":null}},` + comscore
	}
}

// statusBatch renders a whole batch reply from slots.
func statusBatch(slots ...string) string {
	return "[" + strings.Join(slots, ",") + "]"
}

// refuseTheToken is the reply rule of a dead token: any request carrying an
// Authorization header is refused, any request without one is answered.
func refuseTheToken(answer string) func(string) (int, string) {
	return func(auth string) (int, string) {
		if auth != "" {
			return http.StatusUnauthorized, statusRefusalBody
		}
		return http.StatusOK, answer
	}
}

// statusLog keeps every line by level, with its args, so a test can assert
// both how often each was written and that none carries the token.
type statusLog struct {
	mu    sync.Mutex
	lines map[string][]string
}

func (l *statusLog) add(level, msg string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lines == nil {
		l.lines = map[string][]string{}
	}
	l.lines[level] = append(l.lines[level], msg+" "+fmt.Sprint(args...))
}

func (l *statusLog) Debug(msg string, args ...any) { l.add("debug", msg, args) }
func (l *statusLog) Info(msg string, args ...any)  { l.add("info", msg, args) }
func (l *statusLog) Warn(msg string, args ...any)  { l.add("warn", msg, args) }
func (l *statusLog) Error(msg string, args ...any) { l.add("error", msg, args) }

func (l *statusLog) at(level string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines[level]...)
}

// statusService builds a Service over a jar written from rows, with a
// recording log and a recording onAuthLoss.
func statusService(t *testing.T, rows ...string) (*Service, *statusLog, *[]string) {
	t.Helper()
	log := &statusLog{}
	svc := NewService(hlsJar(t, filepath.Join(t.TempDir(), "cookies.txt"), rows...), log)
	var reasons []string
	svc.SetOnAuthLoss(func(reason string) { reasons = append(reasons, reason) })
	return svc, log, &reasons
}

// TestStatusChecksRetrySignedOutWhenTheTokenIsRefused is the wave's first
// offline test, through both entry points that send the batch: the monitor's
// GetStreamInfoBatch and GetStreamInfo (the manual wait, the add route and the
// capture's end checks).
//
// For each: the refused authenticated request, then ONE more with no
// Authorization header and the same body, whose answers are what the caller
// gets — classified as they would have been (live, offline, not found) — and
// one report of the loss with the stream-check reason.
//
// Mutants: the retry deleted (the refusal reaches the caller and two requests
// become one); the repeat sent with the token again (its Authorization header
// is set); a different body for the repeat (the bodies differ); the report
// dropped or sent with another reason (the reasons differ).
func TestStatusChecksRetrySignedOutWhenTheTokenIsRefused(t *testing.T) {
	answer := statusBatch(statusSlot("liveone", "live"), statusSlot("offone", "offline"), statusSlot("goneone", "missing"))
	for _, tc := range []struct {
		name  string
		reply string
		call  func(*Service) error
	}{
		{
			name:  "the monitor's batch",
			reply: answer,
			call: func(svc *Service) error {
				infos, errs, err := svc.GetStreamInfoBatch(context.Background(), []string{"liveone", "offone", "goneone"})
				if err != nil {
					return fmt.Errorf("wholeErr = %v, want the signed-out answer", err)
				}
				if infos[0] == nil || !infos[0].IsLive || errs[0] != nil {
					return fmt.Errorf("liveone = %+v, %v; want live", infos[0], errs[0])
				}
				if infos[1] != nil || errs[1] != nil {
					return fmt.Errorf("offone = %+v, %v; want offline", infos[1], errs[1])
				}
				if !errors.Is(errs[2], ErrChannelNotFound) {
					return fmt.Errorf("goneone = %+v, %v; want ErrChannelNotFound", infos[2], errs[2])
				}
				return nil
			},
		},
		{
			name:  "the single lookup",
			reply: statusBatch(statusSlot("liveone", "live")),
			call: func(svc *Service) error {
				if info, err := svc.GetStreamInfo(context.Background(), "liveone"); err != nil || info == nil || !info.IsLive {
					return fmt.Errorf("GetStreamInfo = %+v, %v; want live", info, err)
				}
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := installStatusStub(t, refuseTheToken(tc.reply))
			svc, _, reasons := statusService(t, row(".twitch.tv", "auth-token", statusTestToken))

			if err := tc.call(svc); err != nil {
				t.Fatal(err)
			}
			seen := stub.requests()
			if len(seen) != 2 {
				t.Fatalf("%d GQL requests, want 2 — the refused one and one signed-out repeat", len(seen))
			}
			if seen[0].auth != "OAuth "+statusTestToken {
				t.Errorf("the first request did not carry the saved token — it must go first, so a repaired cookie file is used at once")
			}
			if seen[1].auth != "" {
				t.Errorf("the repeat carried an Authorization header — it must go signed out")
			}
			if seen[0].body != seen[1].body {
				t.Errorf("the repeat sent a different batch:\n first %s\nrepeat %s", seen[0].body, seen[1].body)
			}
			if want := []string{AuthDowngradeStatusTokenRefused}; fmt.Sprint(*reasons) != fmt.Sprint(want) {
				t.Errorf("reported %q, want %q — the retry must not make the dead login invisible", *reasons, want)
			}
		})
	}
}

// TestStatusChecksRetryOnlyARefusedToken pins the three guards around the
// repeat, each through the real request sequence:
//
//   - no token sent: a cookieless install's refusal is not about a token, so
//     there is nothing to drop — one request, nothing reported.
//   - the signed-out repeat refused as well: the refusal was not about the
//     token, so it is returned as it was (ErrTwitchAuthExpired, which every
//     caller already classifies) and NOT reported.
//   - any other failure (a 400) and a success: one request, nothing reported.
//
// Mutants: the authToken guard dropped (a cookieless install sends a second
// request); the report moved ahead of the repeat's outcome (the refused repeat
// reports); the repeat's error returned instead of the original refusal (the
// error is no longer ErrTwitchAuthExpired); the trigger widened past
// ErrTwitchAuthExpired (the 400 repeats).
func TestStatusChecksRetryOnlyARefusedToken(t *testing.T) {
	tokenRow := row(".twitch.tv", "auth-token", statusTestToken)
	for _, tc := range []struct {
		name        string
		rows        []string
		reply       func(string) (int, string)
		wantCalls   int
		wantExpired bool // the error the caller gets is ErrTwitchAuthExpired
		wantErr     bool
	}{
		{
			name:      "a cookieless install refused",
			reply:     func(string) (int, string) { return http.StatusUnauthorized, statusRefusalBody },
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:        "refused signed in and signed out alike",
			rows:        []string{tokenRow},
			reply:       func(string) (int, string) { return http.StatusForbidden, `{"error":"Forbidden"}` },
			wantCalls:   2,
			wantExpired: true,
			wantErr:     true,
		},
		{
			name:      "a malformed request",
			rows:      []string{tokenRow},
			reply:     func(string) (int, string) { return http.StatusBadRequest, `{"error":"Bad Request"}` },
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:      "the token taken",
			rows:      []string{tokenRow},
			reply:     func(string) (int, string) { return http.StatusOK, statusBatch(statusSlot("offone", "offline")) },
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := installStatusStub(t, tc.reply)
			svc, _, reasons := statusService(t, tc.rows...)

			_, _, err := svc.GetStreamInfoBatch(context.Background(), []string{"offone"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("wholeErr = %v, want an error: %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrTwitchAuthExpired); got != tc.wantExpired {
				t.Errorf("errors.Is(err, ErrTwitchAuthExpired) = %v, want %v (err = %v)", got, tc.wantExpired, err)
			}
			if n := len(stub.requests()); n != tc.wantCalls {
				t.Errorf("%d GQL requests, want %d", n, tc.wantCalls)
			}
			if len(*reasons) != 0 {
				t.Errorf("reported %q, want nothing — no refusal of the token was proven", *reasons)
			}
		})
	}
}

// TestStatusTokenRefusalIsWarnedOncePerRun: the monitor asks every 15 s, so a
// Warn per refused check would bury the log. The first refusal of a run
// Warns, the repeats are Debug, a check Twitch takes the token on again ends
// the run with one Info, and the next refusal Warns afresh. The report is on
// every refusal (NoteTwitchAuthLoss is idempotent). No line carries the token.
//
// Mutants: Warn on every refusal (three Warns); the latch never reset (no
// second Warn, no Info); the report latched with the Warn (one reason, not
// four).
func TestStatusTokenRefusalIsWarnedOncePerRun(t *testing.T) {
	refused := true
	answer := statusBatch(statusSlot("offone", "offline"))
	installStatusStub(t, func(auth string) (int, string) {
		if auth != "" && refused {
			return http.StatusUnauthorized, statusRefusalBody
		}
		return http.StatusOK, answer
	})
	svc, log, reasons := statusService(t, row(".twitch.tv", "auth-token", statusTestToken))
	check := func() {
		t.Helper()
		if _, _, err := svc.GetStreamInfoBatch(context.Background(), []string{"offone"}); err != nil {
			t.Fatalf("GetStreamInfoBatch: %v", err)
		}
	}

	for range 3 {
		check()
	}
	if n := len(log.at("warn")); n != 1 {
		t.Errorf("%d Warn lines for three refused checks, want 1: %q", n, log.at("warn"))
	}
	refused = false
	check()
	if infos := log.at("info"); len(infos) != 1 || !strings.Contains(infos[0], "accepted the saved auth-token") {
		t.Errorf("Info lines after the token was taken again = %q, want the one recovery line", infos)
	}
	refused = true
	check()
	if n := len(log.at("warn")); n != 2 {
		t.Errorf("%d Warn lines after a refusal that followed a recovery, want 2", n)
	}
	// An empty batch sends no request, so it is no news about the token.
	// Mutant: the len(channelLogins) guard dropped — the recovery line is
	// written for a check that was never made.
	if _, _, err := svc.GetStreamInfoBatch(context.Background(), nil); err != nil {
		t.Fatalf("empty GetStreamInfoBatch: %v", err)
	}
	if infos := log.at("info"); len(infos) != 1 {
		t.Errorf("Info lines after an empty batch = %q, want still only the one recovery line", infos)
	}
	if len(*reasons) != 4 {
		t.Errorf("%d reports for four refused checks, want 4", len(*reasons))
	}
	// A check sent with NO token is no news about the saved one: the operator
	// removed the cookies, and Twitch answering signed out must not read as
	// "accepted the saved auth-token again". Mutant: the authToken guard
	// dropped from streamInfo — the recovery line is written.
	hlsJarFile(t, svc.Auth.cookieJar.GetFilePath())
	if err := svc.Auth.cookieJar.Reload(); err != nil {
		t.Fatalf("reload jar: %v", err)
	}
	check()
	if infos := log.at("info"); len(infos) != 1 {
		t.Errorf("Info lines after a cookieless check = %q, want still only the one recovery line", infos)
	}
	for _, level := range []string{"debug", "info", "warn"} {
		for _, line := range log.at(level) {
			if strings.Contains(line, statusTestToken) {
				t.Errorf("a %s line carried the token: %q", level, line)
			}
		}
	}
}

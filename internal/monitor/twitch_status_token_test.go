package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitchMonitorKeepsCheckingWhenTheTokenIsRefused is G21's monitor half
// (owner decision D19), end to end: a REAL twitch.Service over a jar holding
// an auth-token, its GQL endpoint pointed at a local server that refuses any
// request carrying that token with streamlink's dead-token 401 and answers the
// same batch sent without it.
//
// Before, that refusal was a whole-batch error: checkChunk logged "Twitch
// channels are not being checked until it recovers" and classified nothing —
// no live channel found, no health recorded — for as long as the dead token
// sat in cookies.txt. Now the Service repeats the batch signed out, and the
// monitor classifies every channel exactly as a healthy token would have let
// it: the live one dispatched, the offline one checked, the missing one on its
// health streak; and the loss is still reported, so the platform keeps reading
// as needing a login.
//
// Mutant: the Service's signed-out repeat deleted — OnStreamFound never fires,
// no channel's health is recorded, and nothing is reported.
func TestTwitchMonitorKeepsCheckingWhenTheTokenIsRefused(t *testing.T) {
	const token = "test-token-aaaa"
	answer := "[" + strings.Join([]string{
		`{"data":{"user":{"id":"1","displayName":"tw0","login":"tw0","profileImageURL":"",` +
			`"stream":{"id":"9001","title":"on air","type":"live","viewersCount":3,"createdAt":"2026-10-31T00:00:00Z","game":null}}}}`,
		`{"data":{"user":{"stream":null}}}`,
		`{"data":{"user":{"id":"2","displayName":"tw1","login":"tw1","profileImageURL":"","stream":null}}}`,
		`{"data":{"user":{"stream":null}}}`,
		`{"data":{"user":null}}`,
		`{"data":{"user":{"stream":null}}}`,
	}, ",") + "]"
	var (
		mu   sync.Mutex
		auth []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Unauthorized","status":401,"message":"The \"Authorization\" token is invalid."}`)
			return
		}
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	prevGQL := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = prevGQL })

	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte("# Netscape HTTP Cookie File\n"+
		".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\t"+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := twitch.NewService(jar, log)
	var reasons []string
	svc.SetOnAuthLoss(func(reason string) { reasons = append(reasons, reason) })

	chans := twitchChans(3)
	tm := NewTwitchMonitor(config.NewStore(config.Defaults(), ""), newTestDB(t), svc, log)
	_ = tm.configStore.Update(func(c *config.MoomboxConfig) { c.Channels = chans })
	var found []string
	tm.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		found = append(found, ch.ID+"/"+info.StreamID)
	}

	tm.checkChunk(context.Background(), chans)

	if fmt.Sprint(found) != "[tw0/9001]" {
		t.Errorf("OnStreamFound = %v, want [tw0/9001] — the refused token stopped live detection", found)
	}
	if n := tm.batchFailStreak.Load(); n != 0 {
		t.Errorf("batchFailStreak = %d, want 0 — the batch was answered", n)
	}
	health := map[string]ChannelHealth{}
	for _, h := range tm.Health() {
		health[h.ChannelID] = h
	}
	for _, id := range []string{"tw0", "tw1"} {
		if h, ok := health[id]; !ok || h.LastCheckedAt == 0 || h.ConsecutiveErrors != 0 {
			t.Errorf("health[%s] = %+v (recorded: %v), want a successful check", id, h, ok)
		}
	}
	if h := health["tw2"]; h.ConsecutiveErrors != 1 || !strings.Contains(h.LastError, "not found") {
		t.Errorf("health[tw2] = %+v, want one not-found error on its streak", h)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 || auth[0] != "OAuth "+token || auth[1] != "" {
		t.Errorf("Authorization headers sent = %q, want the token once and then none", auth)
	}
	if fmt.Sprint(reasons) != fmt.Sprint([]string{twitch.AuthDowngradeStatusTokenRefused}) {
		t.Errorf("reported %q, want the stream-check reason once", reasons)
	}
}

package bgutils

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

type mintLineLogger struct {
	mu    sync.Mutex
	lines [][]any
}

func (l *mintLineLogger) Debug(string, ...any) {}
func (l *mintLineLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, append([]any{msg}, args...))
}
func (l *mintLineLogger) Warn(string, ...any)  {}
func (l *mintLineLogger) Error(string, ...any) {}

// TestPotProviderLogsEachMintWithITBytes pins the "[POT] mint" line (owner
// ruling RV-18): a player token minted on the goja path's cached minter is
// logged once with its integrity token's length, read under the binding it
// was minted under; the session-cache hit that answers the same binding next
// is no mint and logs nothing; and the line holds no token and no binding.
//
// Mutants (each run): logging on a cache hit fails the count; reading itBytes
// under no binding fails the value; dropping the line fails the count.
func TestPotProviderLogsEachMintWithITBytes(t *testing.T) {
	const visitor = "CgtWaXNpdG9yRGF0YSiAgICAgICAgAEyCgoCVVMSBBoAIDQ%3D"
	logs := &mintLineLogger{}
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, logs)
	r := rand.New(rand.NewPCG(3, 4))
	var tok string
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		ExpiresAt: time.Now().Add(time.Hour),
		MintFunc: func(binding string) (string, error) {
			tok = base64.URLEncoding.EncodeToString(mintShaped(r, 67, binding))
			return tok, nil
		},
	}
	for range 2 {
		if _, err := pp.GeneratePoToken(context.Background(), visitor, false); err != nil {
			t.Fatalf("GeneratePoToken: %v", err)
		}
	}
	var mints [][]any
	for _, l := range logs.lines {
		if l[0] == "[POT] mint" {
			mints = append(mints, l)
		}
	}
	if len(mints) != 1 {
		t.Fatalf("[POT] mint lines = %v, want exactly one (the cache hit is no mint)", mints)
	}
	kv := map[string]any{}
	for i := 1; i+1 < len(mints[0]); i += 2 {
		kv[mints[0][i].(string)] = mints[0][i+1]
	}
	if kv["itBytes"] != 67 || kv["tokenLength"] != len(tok) || kv["bypassCache"] != false || kv["sidecar"] != false {
		t.Errorf("[POT] mint line = %v, want itBytes=67 tokenLength=%d bypassCache=false sidecar=false", kv, len(tok))
	}
	if s := fmt.Sprint(mints[0]); strings.Contains(s, tok) || strings.Contains(s, visitor) {
		t.Errorf("[POT] mint line carries the token or the binding: %v", mints[0])
	}
}

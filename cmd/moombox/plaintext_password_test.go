package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// plaintextPasswordLogger keeps the Warn lines hashPlaintextPassword writes.
type plaintextPasswordLogger struct{ warns []string }

func (l *plaintextPasswordLogger) Info(string, ...any)          {}
func (l *plaintextPasswordLogger) Error(string, ...any)         {}
func (l *plaintextPasswordLogger) Warn(msg string, args ...any) { l.warns = append(l.warns, msg) }

// TestHashPlaintextPasswordWarnsAboutOneTooLongToLogIn pins W26-10's last
// surface: a plaintext password written into config.toml by hand is hashed at
// boot whatever its length, and one over 128 bytes then could never log in.
// It is still hashed — left as plaintext it could not even be changed — and
// exactly as written, but the boot says why the login will refuse it, in the
// words every interactive surface refuses it with.
//
// Mutant killed: dropping the over-length Warn.
func TestHashPlaintextPasswordWarnsAboutOneTooLongToLogIn(t *testing.T) {
	auth := web.NewAuthService()
	for _, tc := range []struct {
		pw   string
		warn bool
	}{
		{strings.Repeat("a", 129), true},
		{strings.Repeat("a", 128), false},
	} {
		cfg := config.Defaults()
		cfg.Network.PasswordHash = tc.pw
		store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))
		l := &plaintextPasswordLogger{}

		hashPlaintextPassword(l, store)

		var hash string
		store.Read(func(c *config.MoomboxConfig) { hash = c.Network.PasswordHash })
		if !auth.VerifyPassword(tc.pw, hash) {
			t.Errorf("%d bytes: the stored hash does not verify the password as written", len(tc.pw))
		}
		warned := strings.Contains(strings.Join(l.warns, "\n"), config.PasswordTooLongMsg)
		if warned != tc.warn {
			t.Errorf("%d bytes: warned %v, want %v (warns %q)", len(tc.pw), warned, tc.warn, l.warns)
		}
	}
}

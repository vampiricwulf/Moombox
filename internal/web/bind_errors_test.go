package web

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestBindErrorWordsTheCause: every failed bind used to be reported as
// "port N (and nearby ports) already in use", which on Linux sent an ordinary
// user on the default port 774 (permission denied below 1024) looking for a
// process that does not exist. The first error's kind now picks the words.
//
// Mutants: the in-use arm keyed on the permission check (or not at all); the
// permission arm missing; the neutral arm claiming "already in use".
func TestBindErrorWordsTheCause(t *testing.T) {
	// A real taken port on this platform: Winsock's WSAEADDRINUSE and the
	// POSIX EADDRINUSE are different errnos.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, inUse := net.Listen("tcp", held.Addr().String())
	if inUse == nil {
		t.Skip("this platform let a second listener share the port")
	}
	denied := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)}
	other := &net.OpError{Op: "listen", Net: "tcp", Err: errors.New("cannot assign requested address")}

	for _, c := range []struct {
		name   string
		err    error
		want   string
		refuse string
	}{
		{"in use", inUse, "port 774 (and nearby ports) already in use", "CAP_NET_BIND_SERVICE"},
		{"permission denied", denied, "cannot bind port 774: permission denied - on Linux ports below 1024 need CAP_NET_BIND_SERVICE; set network.port to 1024 or above", "already in use"},
		{"other", other, "could not bind port 774 (or the ten after it)", "already in use"},
	} {
		got := bindError(774, c.err)
		if !strings.Contains(got.Error(), c.want) {
			t.Errorf("%s: %q, want it to contain %q", c.name, got, c.want)
		}
		if strings.Contains(got.Error(), c.refuse) {
			t.Errorf("%s: %q must not say %q", c.name, got, c.refuse)
		}
		if !errors.Is(got, c.err) {
			t.Errorf("%s: the configured port's own error is not wrapped", c.name)
		}
	}
}

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

// TestListenNearbyWordsTheConfiguredPortsError: with mixed errors (port 1023
// refused for permission, 1024-1033 taken) the error must name the configured
// port's cause, not the last probe's "already in use".
//
// Mutant: listenNearby returning bindError(port, <last probe's error>).
func TestListenNearbyWordsTheConfiguredPortsError(t *testing.T) {
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

	var tried []string
	var probed error
	ln, err := listenNearby("127.0.0.1", 1023, func(addr string) (net.Listener, error) {
		tried = append(tried, addr)
		if addr == "127.0.0.1:1023" {
			return nil, denied
		}
		return nil, inUse
	}, func(first error) { probed = first }, func(int) { t.Error("no port was free") })
	if ln != nil || err == nil {
		t.Fatalf("listenNearby = %v, %v; want an error", ln, err)
	}
	if len(tried) != 11 || tried[10] != "127.0.0.1:1033" {
		t.Errorf("tried %v, want 1023 and the ten after it", tried)
	}
	if probed != denied {
		t.Errorf("onProbe got %v, want the configured port's error", probed)
	}
	if !strings.Contains(err.Error(), "CAP_NET_BIND_SERVICE") || strings.Contains(err.Error(), "already in use") {
		t.Errorf("%q: want the permission wording, not the last probe's", err)
	}
}

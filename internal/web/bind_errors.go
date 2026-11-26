package web

import (
	"errors"
	"fmt"
	"os"
)

// bindFailure is the kind of the first error net.Listen gave for the
// configured port. Server.Start probes the ten ports after it whatever the
// kind (a port of 1014-1023 can still reach an unprivileged one), so the
// kind only decides the words: the log line before the probe and the error
// after it.
type bindFailure int

const (
	bindOther      bindFailure = iota // anything else: worded neutrally
	bindInUse                         // EADDRINUSE / WSAEADDRINUSE: the port is taken
	bindPermission                    // EACCES: on Linux, a port below 1024 without CAP_NET_BIND_SERVICE
)

func classifyBindError(err error) bindFailure {
	switch {
	case isAddrInUse(err):
		return bindInUse
	case errors.Is(err, os.ErrPermission):
		return bindPermission
	}
	return bindOther
}

// probeMessage is the Warn logged before the nearby ports are tried.
func (k bindFailure) probeMessage() string {
	switch k {
	case bindInUse:
		return "port in use, probing nearby ports"
	case bindPermission:
		return "cannot bind port: permission denied, probing nearby ports"
	}
	return "cannot bind port, probing nearby ports"
}

// bindError is what Start returns when the configured port and the ten after
// it all failed. It wraps the configured port's own error, whose text ends in
// the operating system's reason.
func bindError(port int, first error) error {
	switch classifyBindError(first) {
	case bindInUse:
		return fmt.Errorf("port %d (and nearby ports) already in use: %w", port, first)
	case bindPermission:
		return fmt.Errorf("cannot bind port %d: permission denied - on Linux ports below 1024 need CAP_NET_BIND_SERVICE; set network.port to 1024 or above: %w", port, first)
	}
	return fmt.Errorf("could not bind port %d (or the ten after it): %w", port, first)
}

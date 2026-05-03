//go:build !windows

package connectivity

import (
	"net"
	"time"
)

// connectivityProbeAddress is the host:port we TCP-dial to verify
// internet reachability. 1.1.1.1:443 (Cloudflare DNS over HTTPS) is
// anycast-routed, very reliable, and unlikely to be blocked by typical
// firewalls. Port 443 is preferred over DNS port 53 because some
// corporate networks restrict outbound DNS but allow HTTPS.
const connectivityProbeAddress = "1.1.1.1:443"

// connectivityProbeTimeout is how long to wait for the TCP handshake
// before declaring the network down. 2s is generous for slow links
// while not blocking the polling cadence.
const connectivityProbeTimeout = 2 * time.Second

// checkInternetConnected attempts a TCP connection to a reliable
// endpoint. Returns true if the connection succeeds within the timeout.
//
// Unlike Windows' InternetGetConnectedState (a heuristic that doesn't
// actually verify reachability), this is a real reachability check.
// The Monitor's two-poll debounce in poll() smooths over transient
// failures (e.g., one-off packet loss).
func checkInternetConnected() bool {
	conn, err := net.DialTimeout("tcp", connectivityProbeAddress, connectivityProbeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

package worker

import "sync/atomic"

// ConnectivityReporter is the subset of connectivity.Monitor the worker's
// probe loops invoke so their YouTube/Twitch probe outcomes feed the passive
// outage tracker (internal/connectivity/passive.go) — letting a service-only
// failure contribute to the global online/offline signal.
type ConnectivityReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// connReporter is an atomic.Pointer so SetConnectivityReporter is race-free
// against in-flight probes. main wires it once at startup.
var connReporter atomic.Pointer[ConnectivityReporter]

// SetConnectivityReporter wires the package-wide connectivity reporter for the
// worker's probe loops. Safe to call concurrently; nil clears it.
func SetConnectivityReporter(r ConnectivityReporter) {
	if r == nil {
		connReporter.Store(nil)
		return
	}
	connReporter.Store(&r)
}

// reportProbeResult forwards a probe outcome to the installed reporter, if any.
// tag identifies the subsystem ("probe/youtube", "probe/twitch") so the passive
// tracker can count distinct-subsystem failures toward its offline threshold.
func reportProbeResult(tag string, failed bool) {
	rp := connReporter.Load()
	if rp == nil {
		return
	}
	if failed {
		(*rp).ReportFailure(tag)
	} else {
		(*rp).ReportSuccess(tag)
	}
}

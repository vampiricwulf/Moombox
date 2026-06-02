package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestClassifyProbeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want probeErrClass
	}{
		{"nil", nil, classServer},
		{"context canceled", context.Canceled, classCancelled},
		{"deadline exceeded", context.DeadlineExceeded, classNetwork},
		{"url wrapping opError", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}}, classNetwork},
		{"dns error", &net.DNSError{Err: "no such host", Name: "x"}, classNetwork},
		{"unexpected eof", io.ErrUnexpectedEOF, classNetwork},
		{"http 503 string", fmt.Errorf("WEB API error: HTTP 503"), classNetwork},
		{"http 429 string", fmt.Errorf("ANDROID_VR API error: HTTP 429"), classNetwork},
		{"tls string", fmt.Errorf("tls: handshake failure"), classNetwork},
		{"http 404 string", fmt.Errorf("WEB API error: HTTP 404"), classServer},
		{"unknown defaults to network", errors.New("something weird happened"), classNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeErr(tc.err); got != tc.want {
				t.Errorf("classifyProbeErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestProbeErrorDecision(t *testing.T) {
	if d := probeErrorDecision(context.Canceled); !d.cancelled {
		t.Error("cancelled error should yield cancelled decision")
	}
	if d := probeErrorDecision(&net.DNSError{Err: "no such host"}); d.count || d.report != reportFailure {
		t.Errorf("network error: want count=false report=failure, got %+v", d)
	}
	if d := probeErrorDecision(fmt.Errorf("WEB API error: HTTP 404")); !d.count || d.report != reportSuccess {
		t.Errorf("server error: want count=true report=success, got %+v", d)
	}
	if d := probeErrorDecision(errors.New("mystery")); d.count {
		t.Error("unknown error must NOT count (asymmetric default)")
	}
}

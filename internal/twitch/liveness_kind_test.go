package twitch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

// TestRequestFailureKind: the liveness probe's Debug line printed the error's
// Go type, which for every wrapped error was *fmt.wrapError. It now names the
// kind, still without any upstream text.
//
// Mutant: return fmt.Sprintf("%T", err) — every row reads *fmt.wrapError.
func TestRequestFailureKind(t *testing.T) {
	dial := &url.Error{Op: "Post", URL: "https://gql.twitch.tv/gql", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("gql request (op): %w", context.DeadlineExceeded), "timed out"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "cancelled"},
		{fmt.Errorf("gql exhausted 3 retries: %w", fmt.Errorf("gql request (op): %w", dial)), "network error"},
		{fmt.Errorf("gql error (op): %s", "PersistedQueryNotFound"), "Twitch answered with an error"},
	} {
		if got := requestFailureKind(c.err); got != c.want {
			t.Errorf("requestFailureKind(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

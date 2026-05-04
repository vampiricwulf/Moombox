package monitor

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

func TestIsRecoverableTwitchError(t *testing.T) {
	intPtr := func(n int) *int { i := n; return &i }

	tests := []struct {
		name string
		job  *database.Job
		want bool
	}{
		{
			name: "exact flap signature is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          worker.TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: true,
		},
		{
			name: "different error string is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch HLS error: bad request",
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "non-error status is not recoverable",
			job: &database.Job{
				Status:         database.StatusFinished,
				Error:          worker.TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "started downloading is not recoverable (download-time failure)",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          worker.TwitchOfflineErrMsg,
				LastVideoSeq:   intPtr(42),
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "retry budget exhausted is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          worker.TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 2,
			},
			want: false,
		},
		{
			name: "retry budget at 1 of 2 is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          worker.TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 1,
			},
			want: true,
		},
		{
			name: "nil job is not recoverable",
			job:  nil,
			want: false,
		},
	}

	const maxRetries = 2
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRecoverableTwitchError(tt.job, maxRetries)
			if got != tt.want {
				t.Errorf("isRecoverableTwitchError(%+v, %d) = %v, want %v", tt.job, maxRetries, got, tt.want)
			}
		})
	}
}

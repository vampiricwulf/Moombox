package jobstate

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestAwaitingProcessing: a VOD row (IsVod) or a backlog row of a channel
// (queue_priority 1 with a channel id) awaits processing; nothing else does.
// The shared table both UIs run is cmd/moombox TestAwaitingProcessingParity.
//
// Mutants: the channel check dropped; IsVod ignored; an empty channel id
// counted.
func TestAwaitingProcessing(t *testing.T) {
	ch, empty := "UCx", ""
	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"nil", nil, false},
		{"VOD", &database.Job{IsVod: true}, true},
		{"backlog row", &database.Job{QueuePriority: 1, ChannelID: &ch}, true},
		{"priority 1, no channel", &database.Job{QueuePriority: 1}, false},
		{"priority 1, empty channel", &database.Job{QueuePriority: 1, ChannelID: &empty}, false},
		{"a channel's stream", &database.Job{ChannelID: &ch}, false},
	} {
		if got := AwaitingProcessing(tc.job); got != tc.want {
			t.Errorf("%s: AwaitingProcessing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

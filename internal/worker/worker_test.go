package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)


func TestIsTerminalStatus(t *testing.T) {
	tests := []struct {
		status   database.JobStatus
		expected bool
	}{
		{database.StatusFinished, true},
		{database.StatusError, true},
		{database.StatusCancelled, true},
		{database.StatusUpcoming, false},
		{database.StatusLive, false},
		{database.StatusDownloading, false},
		{database.StatusMuxing, false},
		{database.StatusCookies, false},
	}

	for _, tt := range tests {
		result := isTerminalStatus(tt.status)
		if result != tt.expected {
			t.Errorf("isTerminalStatus(%q) = %v, want %v", tt.status, result, tt.expected)
		}
	}
}

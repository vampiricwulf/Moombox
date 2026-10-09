package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

type recordedFrame struct {
	msgType string
	payload any
}

type frameRecorder struct{ frames []recordedFrame }

func (r *frameRecorder) Broadcast(msgType string, payload any) {
	r.frames = append(r.frames, recordedFrame{msgType, payload})
}

// TestTrimEventsGoOutAsTrimStatus: a dashboard trim runs detached from the
// request that started it, so the page learns its outcome only from the
// frame the trim service's hook sends — app.js handles it as "trim_status",
// and any other name is a trim whose result never reaches the page.
//
// Mutant: broadcast under another name ("trim_update").
func TestTrimEventsGoOutAsTrimStatus(t *testing.T) {
	rec := &frameRecorder{}
	ev := worker.TrimEvent{
		TrimTask: worker.TrimTask{ID: "trim_j_1", JobID: "j", StartTime: 1, EndTime: 2},
		State:    worker.TrimStateFailed,
		Error:    "x",
	}
	trimStatusFrames(rec)(ev)
	if len(rec.frames) != 1 || rec.frames[0].msgType != "trim_status" {
		t.Fatalf("frames = %+v, want one trim_status", rec.frames)
	}
	if got, ok := rec.frames[0].payload.(worker.TrimEvent); !ok || got != ev {
		t.Errorf("payload = %#v, want the event itself", rec.frames[0].payload)
	}
}

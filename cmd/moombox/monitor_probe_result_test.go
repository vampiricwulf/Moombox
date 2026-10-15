package main

import (
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Every field the monitors read reaches them, from both probes (one
// adapter serves both). BroadcastEnded is the one that matters most: it is
// what stops an ended broadcast being announced as upcoming every cycle,
// and a dropped copy would silently bring that loop back.
func TestMonitorProbeResultCarriesEveryField(t *testing.T) {
	got := monitorProbeResult(&youtube.VideoInfo{
		StreamStatus:       youtube.StreamUpcoming,
		Title:              "t",
		PublishedAt:        "2026-09-11T02:14:17Z",
		PublishedPrecision: "started",
		PlayabilityError:   youtube.PlayabilityAgeRestricted,
		BroadcastEnded:     true,
	})
	want := &monitor.VideoProbeResult{
		StreamStatus:       "upcoming",
		Title:              "t",
		PublishedAt:        "2026-09-11T02:14:17Z",
		PublishedPrecision: "started",
		PlayabilityError:   "age_restricted",
		BroadcastEnded:     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("monitorProbeResult = %+v, want %+v", got, want)
	}
	// A field added to VideoProbeResult must be added here, and to the
	// adapter: every one is set above, so a zero one means it was missed.
	v := reflect.ValueOf(*got)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("VideoProbeResult.%s is not carried by monitorProbeResult", v.Type().Field(i).Name)
		}
	}
}

package utils

import (
	"math"
	"testing"
)

func TestNewSmoothValue(t *testing.T) {
	tests := []struct {
		name      string
		alpha     float64
		wantAlpha float64
	}{
		{name: "normal alpha 0.5", alpha: 0.5, wantAlpha: 0.5},
		{name: "alpha 0.0", alpha: 0.0, wantAlpha: 0.0},
		{name: "alpha 1.0", alpha: 1.0, wantAlpha: 1.0},
		{name: "alpha clamped from negative", alpha: -0.5, wantAlpha: 0.0},
		{name: "alpha clamped from above 1", alpha: 2.0, wantAlpha: 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sv := NewSmoothValue(tt.alpha)
			if sv.alpha != tt.wantAlpha {
				t.Errorf("expected alpha %v, got %v", tt.wantAlpha, sv.alpha)
			}
			if sv.hasData {
				t.Errorf("expected hasData=false on new SmoothValue")
			}
			if sv.value != 0 {
				t.Errorf("expected initial value 0, got %v", sv.value)
			}
		})
	}
}

func TestSmoothValue_FirstUpdateExact(t *testing.T) {
	sv := NewSmoothValue(0.3)
	result := sv.Update(42.0)
	if result != 42.0 {
		t.Errorf("first update should return exact value, got %v", result)
	}
	if !sv.hasData {
		t.Errorf("expected hasData=true after first update")
	}
	if sv.Value() != 42.0 {
		t.Errorf("Value() should return 42.0 after first update, got %v", sv.Value())
	}
}

func TestSmoothValue_EMAFormula(t *testing.T) {
	alpha := 0.3
	sv := NewSmoothValue(alpha)

	// First update: value = 100
	sv.Update(100.0)

	// Second update: value = alpha*new + (1-alpha)*old = 0.3*200 + 0.7*100 = 60 + 70 = 130
	result := sv.Update(200.0)
	expected := alpha*200.0 + (1-alpha)*100.0
	if math.Abs(result-expected) > 1e-10 {
		t.Errorf("EMA formula: expected %v, got %v", expected, result)
	}

	// Third update: value = 0.3*50 + 0.7*130 = 15 + 91 = 106
	result = sv.Update(50.0)
	expected = alpha*50.0 + (1-alpha)*expected
	if math.Abs(result-expected) > 1e-10 {
		t.Errorf("EMA formula (3rd): expected %v, got %v", expected, result)
	}
}

func TestSmoothValue_Alpha1NoSmoothing(t *testing.T) {
	sv := NewSmoothValue(1.0)

	sv.Update(10.0)
	result := sv.Update(99.0)
	// alpha=1: value = 1.0*99 + 0.0*10 = 99
	if result != 99.0 {
		t.Errorf("alpha=1.0 should track new value exactly, expected 99.0, got %v", result)
	}

	result = sv.Update(0.5)
	if result != 0.5 {
		t.Errorf("alpha=1.0 should track new value exactly, expected 0.5, got %v", result)
	}
}

func TestSmoothValue_Alpha0NeverUpdates(t *testing.T) {
	sv := NewSmoothValue(0.0)

	sv.Update(100.0) // First update sets exact value
	result := sv.Update(999.0)
	// alpha=0: value = 0.0*999 + 1.0*100 = 100
	if result != 100.0 {
		t.Errorf("alpha=0.0 should never change after first, expected 100.0, got %v", result)
	}

	result = sv.Update(0.0)
	if result != 100.0 {
		t.Errorf("alpha=0.0 should never change after first, expected 100.0, got %v", result)
	}
}

func TestSmoothValue_Reset(t *testing.T) {
	sv := NewSmoothValue(0.5)
	sv.Update(100.0)
	sv.Update(200.0)

	sv.Reset()

	if sv.hasData {
		t.Errorf("expected hasData=false after Reset")
	}
	if sv.value != 0 {
		t.Errorf("expected value=0 after Reset, got %v", sv.value)
	}
	if sv.Value() != 0 {
		t.Errorf("expected Value()=0 after Reset, got %v", sv.Value())
	}

	// After reset, next update should be exact again
	result := sv.Update(50.0)
	if result != 50.0 {
		t.Errorf("first update after Reset should be exact, expected 50.0, got %v", result)
	}
}

func TestSmoothValue_ValueBeforeUpdate(t *testing.T) {
	sv := NewSmoothValue(0.5)
	if sv.Value() != 0 {
		t.Errorf("Value() before any update should be 0, got %v", sv.Value())
	}
}

func TestSmoothValue_MultipleUpdatesConverge(t *testing.T) {
	sv := NewSmoothValue(0.5)
	sv.Update(0.0) // start at 0

	// Push toward 100 many times; should converge
	var result float64
	for range 100 {
		result = sv.Update(100.0)
	}

	// After 100 updates with alpha=0.5 toward 100, should be very close to 100
	if math.Abs(result-100.0) > 1e-6 {
		t.Errorf("after 100 updates toward 100.0, expected convergence near 100.0, got %v", result)
	}
}

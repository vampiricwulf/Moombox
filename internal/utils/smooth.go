package utils

// SmoothValue provides exponential moving average smoothing for values
// like download speed and ETA estimation.
type SmoothValue struct {
	alpha   float64
	value   float64
	hasData bool
}

// NewSmoothValue creates a new EMA smoother with the given alpha.
// Alpha should be between 0 and 1. Higher alpha = more responsive to new data.
func NewSmoothValue(alpha float64) *SmoothValue {
	alpha = min(max(alpha, 0), 1)
	return &SmoothValue{alpha: alpha}
}

// Update adds a new data point and returns the smoothed value.
func (s *SmoothValue) Update(newValue float64) float64 {
	if !s.hasData {
		s.value = newValue
		s.hasData = true
		return s.value
	}

	s.value = s.alpha*newValue + (1-s.alpha)*s.value
	return s.value
}

// Value returns the current smoothed value.
func (s *SmoothValue) Value() float64 {
	return s.value
}

// Reset clears the smoother state.
func (s *SmoothValue) Reset() {
	s.value = 0
	s.hasData = false
}

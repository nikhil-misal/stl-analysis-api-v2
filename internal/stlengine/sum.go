package stlengine

import "math"

// neumaierSum implements Neumaier's improved Kahan-Babuska compensated
// summation. It is used to accumulate the per-triangle signed tetrahedron
// volumes without losing precision across hundreds of thousands to millions
// of terms of varying magnitude and sign.
//
// Plain float64 accumulation over ~1e6 terms can lose several significant
// digits to rounding; Neumaier summation keeps the error bounded to
// approximately 1 ULP regardless of term count.
type neumaierSum struct {
	sum float64
	c   float64 // running compensation
}

func (s *neumaierSum) Add(x float64) {
	t := s.sum + x
	if math.Abs(s.sum) >= math.Abs(x) {
		s.c += (s.sum - t) + x
	} else {
		s.c += (x - t) + s.sum
	}
	s.sum = t
}

// Value returns the compensated total.
func (s *neumaierSum) Value() float64 {
	return s.sum + s.c
}

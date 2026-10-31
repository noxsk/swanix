package scheduler

import (
	"fmt"
	"testing"
)

// BenchmarkStep: one scheduling decision for a policy (run every update_interval per policy and family).
func BenchmarkStep(b *testing.B) {
	for _, n := range []int{2, 4, 14} {
		b.Run(fmt.Sprintf("%dwans", n), func(b *testing.B) {
			in := make([]Input, n)
			for i := range in {
				in[i] = loaded(i+1, float64(10+5*i))
			}
			s := New(Params{Saturation: 0.9})
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				in[i%n].RXMbps = float64(i % 100)
				s.Step(in)
			}
		})
	}
}

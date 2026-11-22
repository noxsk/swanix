package scheduler

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

// Diagnostic scenarios for the rejected test-only prototype. These compare
// outcomes without asserting that the prototype must retain its known defects.
// Passing means fixture/partition invariants hold, NOT promotion approval.
func TestBoundedSpillPromotionDiagnostics(t *testing.T) {
	for _, scenario := range []string{"near-threshold", "degraded-receiver", "recovering-receiver", "low-preference-receiver"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := newBoundedSpillExperiment(Params{Saturation: .9})
			ordinary := New(Params{})
			in := []Input{loaded(1, 0), loaded(2, 0)}
			candidate.Step(in)
			ordinary.Step(in)
			in[0].RXMbps, in[1].RXMbps = 95, 10
			switch scenario {
			case "near-threshold":
				in[1].RXMbps = 86
			case "degraded-receiver":
				in[1].State, in[1].LossRate = health.Degraded, .5
			case "recovering-receiver":
				in[1].State, in[1].RecoverRamp = health.Recovering, 30*time.Second
			case "low-preference-receiver":
				in[1].Preference = 1
			}
			var a, b Result
			for i := 0; i < 40; i++ {
				a, b = candidate.Step(in), ordinary.Step(in)
				if total(a.Shares) != 100 || total(b.Shares) != 100 || a.Shares[1] < 1 || a.Shares[2] < 1 {
					t.Fatal("invalid shares", a, b)
				}
			}
			t.Logf("40 identical observations: prototype=%v saturation-off=%v; prototype delta on receiver=%+d points", a.Shares, b.Shares, a.Shares[2]-b.Shares[2])
		})
	}
}

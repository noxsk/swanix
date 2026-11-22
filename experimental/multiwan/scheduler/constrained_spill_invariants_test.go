package scheduler

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

// Clone only the disabled-spill base for an independent one-step ordinary
// objective comparison at exactly the candidate's current smoothing state.
func cloneConstrainedBase(s *Scheduler) *Scheduler {
	copyFloat := func(in map[int]float64) map[int]float64 {
		out := make(map[int]float64, len(in))
		for id, v := range in {
			out[id] = v
		}
		return out
	}
	out := New(s.p)
	out.applied, out.smoothed = copyFloat(s.applied), copyFloat(s.smoothed)
	for id, v := range s.cands {
		out.cands[id] = v
	}
	return out
}

func TestConstrainedSpillDirectionAndPartition(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveBalanced, config.ObjectiveThroughput} {
		t.Run(objective, func(t *testing.T) {
			rng := rand.New(rand.NewSource(20261108))
			s := newConstrainedSpill(Params{Objective: objective, Saturation: .9})
			in := []Input{wan(1, 50, 10), wan(2, 100, 20), wan(3, 500, 50), wan(4, 200, 30)}
			for i := range in {
				in[i].MetricsFresh = true
				in[i].BaseRTT = 10 * time.Millisecond
				in[i].RTT = 10 * time.Millisecond
			}
			s.Step(in)
			capped := 0
			for round := 0; round < 1000; round++ {
				for i := range in {
					in[i].Bias = []string{config.BiasDownload, config.BiasUpload, config.BiasBalanced}[round%3]
					in[i].TXMbps = in[i].UpMbps * (.05 + 1.05*rng.Float64())
					in[i].RXMbps = in[i].DownMbps * (.05 + 1.05*rng.Float64())
					in[i].RTT = time.Duration(10+rng.Intn(200)) * time.Millisecond
					in[i].Preference = 1 + rng.Intn(200)
					in[i].LossRate = .5 * rng.Float64()
				}
				prev := s.base.applied
				oracle := cloneConstrainedBase(s.base)
				oracle.Step(in)
				r := s.Step(in)
				var sum float64
				for id, v := range s.base.applied {
					sum += v
					d, ordinary := v-prev[id], oracle.applied[id]-prev[id]
					// Constraint may slow an objective-directed move, never reverse
					// it, overshoot it, violate floor, or exceed the existing step.
					if d*ordinary < -1e-8 || math.Abs(d) > math.Abs(ordinary)+1e-8 || v < 1-1e-8 || math.Abs(d) > 10+1e-8 {
						t.Fatalf("round=%d id=%d prev=%g ordinary=%g constrained=%g", round, id, prev[id], oracle.applied[id], v)
					}
					if math.Abs(d-ordinary) > 1e-8 {
						capped++
					}
				}
				if math.Abs(sum-100) > 1e-7 || total(r.Shares) != 100 {
					t.Fatal("partition violated", sum, r)
				}
				for id := range r.Saturated {
					if s.base.applied[id] >= prev[id] {
						t.Fatal("receiving path reported as unloading")
					}
				}
				if r.Changed == sameShares(r.Shares, roundShares(prev, 1)) {
					t.Fatal("Changed mismatch")
				}
			}
			if capped == 0 {
				t.Fatal("fixture did not exercise constraints")
			}
			t.Logf("1000 heterogeneous 4-WAN steps, constrained deltas=%d", capped)
		})
	}
}

func TestConstrainedSpillFallbackAndFailover(t *testing.T) {
	for _, change := range []string{"stale", "missing-rtt", "zero-capacity", "replace", "down", "backup", "join", "empty"} {
		t.Run(change, func(t *testing.T) {
			s := newConstrainedSpill(Params{Objective: config.ObjectiveBalanced, Saturation: .9})
			in := heterogeneous()
			s.Step(in)
			in[0].RXMbps = 49
			s.Step(in)
			switch change {
			case "stale":
				in[1].MetricsFresh = false
			case "missing-rtt":
				in[1].RTT = 0
			case "zero-capacity":
				in[1].DownMbps = 0
			case "replace":
				in[1].ID = 3
			case "down":
				in[0].State = health.Down
			case "backup":
				in[0].Role = config.RoleBackup
			case "join":
				w := in[1]
				w.ID = 3
				in = append(in, w)
			case "empty":
				in = nil
			}
			oracle := cloneConstrainedBase(s.base)
			want, got := oracle.Step(in), s.Step(in)
			if !sameShares(want.Shares, got.Shares) || want.Changed != got.Changed || len(s.saturated) != 0 {
				t.Fatal("fallback differs from ordinary scheduler", want, got, s.saturated)
			}
		})
	}
}

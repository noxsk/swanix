package scheduler

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// REJECTED V1, test-only: never wire into policy configuration as-is. This
// prototype ignores health/preference/recovery when it overrides the ordinary
// target, and reseeding after deadband can freeze small corrections. The
// diagnostics in bounded_spill_limits_test.go preserve reproducible evidence
// for the next design, not correctness assertions about those known defects.
// Shares
// are connections, not bytes: total configured capacity is merely the scale for
// converting residual Mbps to percentage points, NOT an offered-demand estimate.
// Compare against the production controller before considering promotion.
type boundedSpillExperiment struct {
	base      *Scheduler
	threshold float64
	saturated map[int]bool
}

func newBoundedSpillExperiment(p Params) *boundedSpillExperiment {
	threshold := p.Saturation
	p.Saturation = 0
	return &boundedSpillExperiment{New(p), threshold, map[int]bool{}}
}

func (b *boundedSpillExperiment) Step(in []Input) Result {
	prev := b.base.applied
	res := b.base.Step(in) // ordinary scoring, EWMA, failover and recovery
	pool := candidates(in)
	fresh, same := len(pool) > 0, len(pool) == len(prev)
	var totalCap float64
	for _, w := range pool {
		fresh = fresh && w.MetricsFresh && capacity(w) > 0
		_, exists := prev[w.ID]
		same = same && exists
		totalCap += capacity(w)
	}
	if !fresh || !same || (b.base.p.Objective != config.ObjectiveCapacity && !comparableMetrics(pool)) {
		b.saturated = map[int]bool{}
		return res
	}
	nextSat := map[int]bool{}
	for _, w := range pool {
		u := Utilisation(w)
		if u >= b.threshold || b.saturated[w.ID] && u >= b.threshold-saturationRelease {
			nextSat[w.ID] = true
		}
	}
	b.saturated = nextSat
	if len(nextSat) == 0 || len(nextSat) == len(pool) {
		return res
	}
	donors, receivers := map[int]float64{}, map[int]float64{}
	var give, room float64
	for _, w := range pool {
		u := Utilisation(w)
		if nextSat[w.ID] {
			v := math.Min(b.base.p.MaxStep, math.Max(0, prev[w.ID]-b.base.p.MinShare))
			v = math.Min(v, 100*capacity(w)*math.Max(0, u-(b.threshold-saturationRelease))/totalCap)
			donors[w.ID], give = v, give+v
		} else {
			v := math.Min(b.base.p.MaxStep, 100*capacity(w)*math.Max(0, b.threshold-u)/totalCap)
			receivers[w.ID], room = v, room+v
		}
	}
	move := math.Min(give, room)
	target := make(map[int]float64, len(prev))
	for id, share := range prev {
		target[id] = share
		if give > 0 {
			target[id] -= move * donors[id] / give
		}
		if room > 0 {
			target[id] += move * receivers[id] / room
		}
	}
	// Preserve the same deadband and reseed smoothing with the actually used
	// shares, rather than retaining an unseen target that fights the controller.
	if maxDelta(target, prev) < b.base.p.Deadband {
		target = prev
	}
	b.base.applied = target
	b.base.smoothed = make(map[int]float64, len(target))
	for id, value := range target {
		b.base.smoothed[id] = value
	}
	res.Shares = roundShares(target, b.base.p.MinShare)
	res.Changed = !sameShares(res.Shares, roundShares(prev, b.base.p.MinShare))
	res.Saturated = nextSat
	return res
}

func TestBoundedSpillFeedbackExperiment(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveBalanced} {
		for _, interval := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s/%ds", objective, interval), func(t *testing.T) {
				p := Params{Objective: objective, Saturation: .9}
				hard := runFeedbackModel(t, p, interval, 100, false)
				bounded := runFeedbackController(t, newBoundedSpillExperiment(p), interval, 100, false, false)
				reversed := runFeedbackController(t, newBoundedSpillExperiment(p), interval, 100, true, false)
				fine := runFeedbackController(t, newBoundedSpillExperiment(p), interval, 1000, false, false)
				noSpill := p
				noSpill.Saturation = 0
				disabled := runFeedbackModel(t, noSpill, interval, 100, false)
				for i, st := range bounded {
					t.Logf("%s hard -> bounded: served %.2f -> %.2f Mbps; path-time queue p95 %.2f -> %.2f ms; late spill transitions %d -> %d; max jump %d -> %d", feedbackPhases[i].name, hard[i].served/60, st.served/60, hard[i].delays[len(hard[i].delays)*95/100], st.delays[len(st.delays)*95/100], hard[i].lateSpillChanges, st.lateSpillChanges, hard[i].maxJump, st.maxJump)
					t.Logf("disabled spill: served %.2f Mbps p95 %.2f ms; candidate 1ms tick: served %.2f Mbps p95 %.2f ms", disabled[i].served/60, disabled[i].delays[len(disabled[i].delays)*95/100], fine[i].served/60, fine[i].delays[len(fine[i].delays)*95/100])
					if !slices.Equal(st.trace, reversed[i].trace) || math.Abs(st.served-reversed[i].served) > 1e-6 {
						t.Fatal("candidate depends on input order")
					}
					// Broad regression bounds, not a promise of improvement at every
					// percentile. Recovery includes backlog, so compare only load phases.
					if i == 1 || i == 2 {
						if st.served < hard[i].served || st.served < .98*disabled[i].served {
							t.Fatal("candidate service regressed against hard or disabled spill")
						}
						if st.lateSpillChanges > hard[i].lateSpillChanges {
							t.Fatal("candidate increased persistent spill transitions")
						}
					}
					if math.Abs(st.served-fine[i].served)/60 > 1 {
						t.Fatal("service differs by >1 Mbps with finer integration")
					}
				}
				if bounded[0].dropped > 1e-6 || bounded[3].endQueue > 1e-6 || bounded[3].dropped > 1e-6 {
					t.Fatal("candidate lost light traffic or failed to drain")
				}
			})
		}
	}
}

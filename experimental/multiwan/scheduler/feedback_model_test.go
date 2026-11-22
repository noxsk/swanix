package scheduler

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// feedbackModel is a deterministic fluid short-transfer model, NOT TCP or a
// router benchmark. New equal-size transfers follow integer shares; previously
// queued bytes never migrate. Each 100-Mbps path has a 50-Mbit drop-tail queue.
// Served Mbit samples at 1/2/5-second intervals (EWMA .3 per sample) and
// queue-derived RTT feed Step. This sensitivity sweep changes both feedback
// delay and smoothing time constant, as a configured sampling interval would.
// Background traffic reserves service first; it is not included in app throughput.
// No packet/ACK contention, slow start, long-flow lifetime, probe failures or
// kernel scheduling is modeled. The fixed offered-load phases are not tuned to
// select winning objectives. Delays sample queues in time, NOT request latency.
type feedbackPhase struct {
	name               string
	demand, background float64
}

type feedbackStats struct {
	served, dropped, offered float64
	startQueue, endQueue     float64
	delays                   []float64
	changes, spillChanges    int
	maxJump                  int
	lateSpillChanges         int      // decision-time transitions in the last 30 seconds
	trace                    [][4]int // per-decision shares and spill flags
}

var feedbackPhases = []feedbackPhase{
	{"idle-headroom", 50, 0},
	{"one-busy", 140, 40},
	{"all-overloaded", 220, 0},
	{"drain-recover", 60, 0},
}

func runFeedbackModel(t *testing.T, p Params, interval, hz int, reverse bool) []feedbackStats {
	t.Helper()
	// Historical experiments retain the pre-soft-spill controller as baseline.
	return runFeedbackController(t, newHardSpillReference(p), interval, hz, reverse, true)
}

type feedbackController interface {
	Step([]Input) Result
}

func runFeedbackController(t *testing.T, s feedbackController, interval, hz int, reverse, hardSpill bool) []feedbackStats {
	t.Helper()
	in := []Input{loaded(1, 0), loaded(2, 0)}
	in[0].BaseRTT, in[1].BaseRTT = 10*time.Millisecond, 30*time.Millisecond
	in[0].RTT, in[1].RTT = in[0].BaseRTT, in[1].BaseRTT
	var queue, servedSample [2]float64
	inputs := func() []Input {
		if reverse {
			return []Input{in[1], in[0]}
		}
		return in
	}
	last := s.Step(inputs())
	dt := 1 / float64(hz)
	stats := make([]feedbackStats, len(feedbackPhases))
	for phase, workload := range feedbackPhases {
		st := &stats[phase]
		st.startQueue = queue[0] + queue[1]
		for tick := 0; tick < 60*hz; tick++ { // 60 seconds per phase
			for line := range queue {
				bg := 0.0
				if line == 0 {
					bg = workload.background
				}
				arrival := workload.demand * dt * float64(last.Shares[line+1]) / 100
				st.offered += arrival
				queued := queue[line] + arrival
				served := math.Min(queued, (100-bg)*dt)
				remaining := queued - served
				st.dropped += math.Max(0, remaining-50)
				queue[line] = math.Min(50, remaining)
				st.served += served
				servedSample[line] += served + bg*dt
				st.delays = append(st.delays, queue[line]/(100-bg)*1000)
			}
			if (tick+1)%(hz*interval) != 0 {
				continue
			}
			for line := range in {
				in[line].RXMbps = .3*servedSample[line]/float64(interval) + .7*in[line].RXMbps
				servedSample[line] = 0
				bg := 0.0
				if line == 0 {
					bg = workload.background
				}
				in[line].RTT = in[line].BaseRTT + time.Duration(queue[line]/(100-bg)*float64(time.Second))
			}
			next := s.Step(inputs())
			decision := [4]int{next.Shares[1], next.Shares[2]}
			for id := range next.Saturated {
				decision[id+1] = 1
			}
			st.trace = append(st.trace, decision)
			if total(next.Shares) != 100 || next.Shares[1] < 1 || next.Shares[2] < 1 {
				t.Fatalf("%s: invalid shares: %+v", workload.name, next)
			}
			newSpill := false
			for id := range next.Saturated {
				if hardSpill && next.Shares[id] != 1 {
					t.Fatalf("%s: saturated floor violated: %+v", workload.name, next)
				}
				newSpill = newSpill || !last.Saturated[id]
			}
			jump := int(math.Abs(float64(next.Shares[1] - last.Shares[1])))
			if jump > 11 && (!hardSpill || !newSpill) {
				t.Fatalf("%s: non-emergency step exceeds bound: %v -> %v", workload.name, last.Shares, next.Shares)
			}
			st.maxJump = max(st.maxJump, jump)
			if next.Changed {
				st.changes++
			}
			if next.Saturated[1] != last.Saturated[1] || next.Saturated[2] != last.Saturated[2] {
				st.spillChanges++
				if tick+1 > 30*hz {
					st.lateSpillChanges++
				}
			}
			last = next
		}
		st.endQueue = queue[0] + queue[1]
		if math.Abs(st.startQueue+st.offered-st.served-st.dropped-st.endQueue) > 1e-6 {
			t.Fatal("model lost Mbit")
		}
		sort.Float64s(st.delays)
	}
	return stats
}

func TestDynamicFeedbackModel(t *testing.T) {
	t.Log("MODEL ONLY: 2x100 Mbps, 50-Mbit queues, 10-ms fluid ticks; 1/2/5-s load/RTT feedback, EWMA .3 per sample; no TCP/ACK/long-flow or real request percentiles")
	for _, tc := range []struct {
		name string
		p    Params
	}{
		{"capacity-no-spill", Params{}},
		{"capacity-spill90", Params{Saturation: .9}},
		{"balanced-no-spill", Params{Objective: config.ObjectiveBalanced}},
		{"balanced-spill90", Params{Objective: config.ObjectiveBalanced, Saturation: .9}},
	} {
		for _, interval := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s/%ds", tc.name, interval), func(t *testing.T) {
				stats := runFeedbackModel(t, tc.p, interval, 100, false)
				reversed := runFeedbackModel(t, tc.p, interval, 100, true)
				fine := runFeedbackModel(t, tc.p, interval, 1000, false)
				for i, st := range stats {
					p95 := st.delays[len(st.delays)*95/100]
					t.Logf("%s: served=%.2f Mbps drop=%.2f Mbit path-time-queue-p95=%.2f ms weight-updates=%d spill-transitions=%d max-jump=%d end-queue=%.2f Mbit", feedbackPhases[i].name, st.served/60, st.dropped, p95, st.changes, st.spillChanges, st.maxJump, st.endQueue)
					t.Logf("late spill transitions=%d; 1ms-tick served=%.2f Mbps late transitions=%d", st.lateSpillChanges, fine[i].served/60, fine[i].lateSpillChanges)
					other := reversed[i]
					if math.Abs(st.served-other.served) > 1e-6 || !slices.Equal(st.trace, other.trace) || !slices.Equal(st.delays, other.delays) {
						t.Fatal("input ordering changed feedback trajectory")
					}
				}
				if stats[0].dropped > 1e-6 || math.Abs(stats[0].served/60-50) > 1e-6 {
					t.Fatal("light load must be fully served")
				}
				last := stats[len(stats)-1]
				if last.endQueue > 1e-6 || last.dropped > 1e-6 {
					t.Fatal("queues failed to drain after overload")
				}
			})
		}
	}
}

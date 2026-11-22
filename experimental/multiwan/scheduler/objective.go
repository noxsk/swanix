package scheduler

import (
	"math"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

func comparableMetrics(cands []Input) bool {
	for _, w := range cands {
		if !w.MetricsFresh || w.BaseRTT <= 0 || w.RTT <= 0 {
			return false
		}
	}
	return true
}

// objectiveScores separates path health from suitability for a workload.
// RTTs must be measured with comparable targets. If ANY usable member lacks
// current evidence, fall back for the whole pool: mixing latency and capacity
// score units would give unmeasured paths an arbitrary advantage.
func (s *Scheduler) objectiveScores(cands []Input) (map[int]float64, map[int]string) {
	scores, reasons := make(map[int]float64, len(cands)), make(map[int]string, len(cands))
	fresh := comparableMetrics(cands)
	minBase, maxBase := time.Duration(0), time.Duration(0)
	for _, w := range cands {
		if minBase == 0 || w.BaseRTT < minBase {
			minBase = w.BaseRTT
		}
		if w.BaseRTT > maxBase {
			maxBase = w.BaseRTT
		}
	}
	objective := s.p.Objective
	if objective == config.ObjectiveCapacity || !fresh {
		for _, w := range cands {
			if objective != config.ObjectiveCapacity {
				// Stable configured weights until the whole pool is comparable. Do not
				// apply RTT/headroom to some lines but not to others during cold start.
				w.MetricsFresh = false
				reasons[w.ID] = "objective fallback: incomplete fresh load/RTT evidence"
			}
			scores[w.ID] = Score(w)
		}
		return scores, reasons
	}
	// Reserve only if there is a meaningfully slower alternative. A 5 ms / 25%
	// band groups near-equal low-latency paths and avoids reserving every member.
	band := time.Duration(math.Max(float64(5*time.Millisecond), float64(minBase)/4))
	reserve := objective == config.ObjectiveThroughput && s.p.LatencyReservePercent > 0 && maxBase-minBase > band
	for _, w := range cands {
		// Reuse preference, health and recovery ramp, but not capacity or the old
		// relative RTT penalty. Load and absolute RTT are accounted for below.
		base := w
		base.MetricsFresh = false
		cap := capacity(w)
		if cap <= 0 {
			scores[w.ID] = 0
			continue
		}
		factor := Score(base) / cap * math.Pow(1-clamp(w.LossRate, 0, .5), 2)
		free := math.Max(.01, 1-clamp(Utilisation(w), 0, 1))
		if reserve && w.BaseRTT-minBase <= band {
			free = math.Max(.01, free-float64(s.p.LatencyReservePercent)/100)
			reasons[w.ID] = "throughput: soft headroom reserved on low-latency path"
		}
		// A single good sample must not make an otherwise slow path look faster
		// than its baseline. Queueing RTT is a workload cost, not a health failure.
		rtt := math.Max(float64(w.BaseRTT), float64(w.RTT)) / float64(time.Second)
		rtt = math.Max(.001, rtt)
		switch objective {
		case config.ObjectiveLatency:
			// No direct capacity multiplier: a wide slow path should not steal small
			// requests solely because of its configured bandwidth.
			scores[w.ID] = factor * math.Pow(free, .7) / (rtt * rtt)
			reasons[w.ID] = "latency: response delay with load headroom"
		case config.ObjectiveThroughput:
			scores[w.ID] = factor * cap * free
			if reasons[w.ID] == "" {
				reasons[w.ID] = "throughput: remaining directional capacity"
			}
		case config.ObjectiveBalanced:
			// Engineering proxy for completion time: two RTTs plus serialization of
			// the configured reference transfer over residual directional capacity.
			// Not a TCP model; multiplexing, slow start and per-flow fair share differ.
			seconds := 2*rtt + float64(s.p.FlowSizeKiB)*1024*8/(cap*free*1e6)
			scores[w.ID] = factor / (seconds * seconds)
			reasons[w.ID] = "balanced: reference transfer completion cost"
		default:
			scores[w.ID] = Score(w)
		}
	}
	return scores, reasons
}

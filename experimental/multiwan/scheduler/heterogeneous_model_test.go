package scheduler

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// This is a fixed-policy-snapshot, discrete-event model, NOT a real-device,
// TCP, or full feedback-loop test. Each path is an ideal equal processor-sharing
// byte server; completion adds two baseline RTTs, independently of serialization.
// No slow start, packet loss, protocol overhead, shaping, or dynamic probes are
// modeled. The generator knows actual sizes and class labels: the scheduler does
// not discover either. A class selects a preconfigured policy, never a size oracle.
// Soft reservation changes connection probabilities, not server capacity/priority.
type heterogeneousRequest struct {
	arrival, bits, draw float64
	class               int // 0 = short, 1 = bulk
}

type heterogeneousStats struct {
	latency    [2][]float64
	counts     [2][2]int
	bits       [2][2]float64
	unfinished [2]int // at the end of the arrival window, before draining
	bulkMbps   float64
	bulkMean   float64
}

func heterogeneousDemand(load float64) []heterogeneousRequest {
	rng := rand.New(rand.NewSource(20260421))
	shortKiB := [...]float64{4, 16, 64}
	bulkKiB := [...]float64{1024, 4096, 8192}
	// 90% short connections; arrival rate derives from the exact distribution's
	// expected bits and offered load relative to the 550 decimal-Mbps pool.
	meanBits := (.9*(4+16+64)/3 + .1*(1024+4096+8192)/3) * 1024 * 8
	rate := load * 550e6 / meanBits
	requests := make([]heterogeneousRequest, 12000)
	var arrival float64
	for i := range requests {
		arrival += rng.ExpFloat64() / rate
		class := 0
		size := shortKiB[rng.Intn(len(shortKiB))]
		if rng.Float64() < .1 {
			class = 1
			size = bulkKiB[rng.Intn(len(bulkKiB))]
		}
		requests[i] = heterogeneousRequest{arrival, size * 1024 * 8, rng.Float64(), class}
	}
	return requests
}

func runHeterogeneousModel(requests []heterogeneousRequest, paths []Input, shares [2]map[int]int) heterogeneousStats {
	type transfer struct {
		request   heterogeneousRequest
		remaining float64
	}
	var active [2][]transfer
	var stats heterogeneousStats
	var now, bulkEnd float64
	windowEnd := requests[len(requests)-1].arrival
	complete := func(r heterogeneousRequest, finished float64) {
		stats.latency[r.class] = append(stats.latency[r.class], finished-r.arrival)
		if finished > windowEnd {
			stats.unfinished[r.class]++
		}
		if r.class == 1 {
			bulkEnd = math.Max(bulkEnd, finished)
		}
	}
	advance := func(until float64) {
		for {
			next := math.Inf(1)
			for line, queue := range active {
				for _, flow := range queue {
					next = math.Min(next, flow.remaining*float64(len(queue))/(paths[line].DownMbps*1e6))
				}
			}
			if math.IsInf(next, 1) {
				now = until
				return
			}
			dt := math.Min(next, until-now)
			now += dt
			for line, queue := range active {
				if len(queue) == 0 {
					continue
				}
				served := dt * paths[line].DownMbps * 1e6 / float64(len(queue))
				remaining := queue[:0]
				for _, flow := range queue {
					flow.remaining -= served
					if flow.remaining <= 1e-4 { // sub-bit floating-point tolerance
						complete(flow.request, now+2*paths[line].BaseRTT.Seconds())
					} else {
						remaining = append(remaining, flow)
					}
				}
				active[line] = remaining
			}
			if now >= until {
				return
			}
		}
	}
	for _, r := range requests {
		advance(r.arrival)
		line := 1
		if r.draw*100 < float64(shares[r.class][paths[0].ID]) {
			line = 0
		}
		active[line] = append(active[line], transfer{r, r.bits})
		stats.counts[r.class][line]++
		stats.bits[r.class][line] += r.bits
	}
	// Drain every admitted byte, including overloaded cases; no timeout/drop or
	// survivor-only percentiles. Late completions remain in all statistics.
	advance(math.Inf(1))
	stats.bulkMbps = (stats.bits[1][0] + stats.bits[1][1]) / (bulkEnd - requests[0].arrival) / 1e6
	for _, seconds := range stats.latency[1] {
		stats.bulkMean += seconds / float64(len(stats.latency[1]))
	}
	for class := range stats.latency {
		sort.Float64s(stats.latency[class])
	}
	return stats
}

func TestHeterogeneousFixedSnapshotModel(t *testing.T) {
	t.Log("MODEL ONLY: fixed idle/fresh policy snapshot, ideal equal byte sharing + 2 baseline RTTs; known generated class/size, not inferred; no TCP/device/full feedback")
	// Predeclared light/medium/high offered loads, not a sweep selecting winners.
	for _, tc := range []struct {
		name string
		load float64
	}{{"light", .2}, {"medium", .6}, {"high", .9}} {
		t.Run(tc.name, func(t *testing.T) {
			paths := heterogeneous()
			legacy := New(Params{Objective: config.ObjectiveCapacity}).Step(paths).Shares
			policies := [2][2]map[int]int{
				{legacy, legacy},
				{New(Params{Objective: config.ObjectiveLatency}).Step(paths).Shares,
					New(Params{Objective: config.ObjectiveThroughput, LatencyReservePercent: 50}).Step(paths).Shares},
			}
			requests := heterogeneousDemand(tc.load)
			var results [2]heterogeneousStats
			quantile := func(s heterogeneousStats, p float64) float64 {
				return s.latency[0][int(math.Ceil(p*float64(len(s.latency[0]))))-1]
			}
			for strategy, name := range []string{"legacy capacity/capacity", "new latency/throughput reserve=50%"} {
				s := runHeterogeneousModel(requests, paths, policies[strategy])
				results[strategy] = s
				if len(s.latency[0])+len(s.latency[1]) != len(requests) {
					t.Fatal("model lost requests")
				}
				t.Logf("%s offered=%.0f%% short completion p50/p95/p99=%.3f/%.3f/%.3f ms; bulk effective=%.3f Mbps mean completion=%.3f s; completed short/bulk=%d/%d; unfinished at arrival-window end=%v, after drain=0",
					name, tc.load*100, quantile(s, .5)*1000, quantile(s, .95)*1000, quantile(s, .99)*1000,
					s.bulkMbps, s.bulkMean, len(s.latency[0]), len(s.latency[1]), s.unfinished)
				for class, label := range []string{"short", "bulk"} {
					for line, path := range paths {
						t.Logf("%s %s WAN%d: policy=%d%% realized connections=%.2f%% bytes=%.2f%%", name, label, path.ID,
							policies[strategy][class][path.ID], 100*float64(s.counts[class][line])/float64(len(s.latency[class])),
							100*s.bits[class][line]/(s.bits[class][0]+s.bits[class][1]))
					}
				}
			}
			old, next := results[0], results[1]
			// Require median and p95 improvement, not a claim that every percentile
			// must improve: the nonzero slow-path share can still dominate p99.
			// In particular high load can worsen p99; report it, do not select
			// another seed/load/reservation to hide that fixed-snapshot limitation.
			if quantile(next, .5) >= quantile(old, .5) || quantile(next, .95) >= quantile(old, .95) {
				t.Error("short median/p95 did not improve")
			}
			if next.bulkMbps < .8*old.bulkMbps || next.bulkMean > 1.5*old.bulkMean {
				t.Error("bulk regression exceeds guardrails: 20% effective throughput or 50% mean completion")
			}
		})
	}
}

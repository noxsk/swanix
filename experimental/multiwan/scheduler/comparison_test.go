package scheduler

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// legacyScore freezes the pre-optimization score (16a0115) independently of
// Score, so the comparison cannot silently start benchmarking itself.
func legacyScore(in Input) float64 {
	pref := float64(in.Preference) / 100
	if pref <= 0 {
		pref = 1
	}
	base := capacity(in) * pref * healthFactor(in.State)
	if !in.MetricsFresh {
		return base
	}
	u := math.Max(in.RXMbps/in.DownMbps, in.TXMbps/in.UpMbps)
	headroom := math.Pow(math.Max(.1, 1-clamp(u, 0, 1)), .7)
	lat := 1.0
	if u < .2 && in.BaseRTT > 0 && in.RTT > in.BaseRTT {
		lat = 1 / (1 + float64(in.RTT-in.BaseRTT)/(2*math.Max(float64(in.BaseRTT), float64(10*time.Millisecond))))
	}
	return base * headroom * lat * math.Pow(1-clamp(in.LossRate, 0, .5), 2)
}

func TestScoreBiasConsistency(t *testing.T) {
	for _, bias := range []string{config.BiasDownload, config.BiasUpload} {
		a := loaded(1, 50)
		a.Bias = bias
		a.TXMbps = 5
		b := a
		if bias == config.BiasDownload {
			b.TXMbps = 9.5
		} else {
			b.RXMbps = 95
		}
		if Score(a) != Score(b) {
			t.Fatalf("%s: opposite direction changed headroom %g -> %g", bias, Score(a), Score(b))
		}
		b.Bias = config.BiasBalanced
		a.Bias = config.BiasBalanced
		if Score(b) >= Score(a) {
			t.Fatal("balanced must still consider the busier direction")
		}
	}
}

func TestEmptyPoolDoesNotKeepChanging(t *testing.T) {
	s := New(Params{Saturation: .9})
	if s.Step(nil).Changed {
		t.Fatal("initial empty pool has no change")
	}
	s.Step([]Input{loaded(1, 95), loaded(2, 10)})
	if !s.Step(nil).Changed {
		t.Fatal("removal must be published")
	}
	if s.Step(nil).Changed {
		t.Fatal("unchanged empty pool must not trigger kernel writes")
	}
	if len(s.saturated) != 0 {
		t.Fatal("stale saturation survived outage")
	}
}

// fixedShareQueue is deliberately a fluid-model regression, NOT network
// throughput. Two 100-Mbit/s servers, 180 Mbit/s equal-size short-flow demand,
// 50-Mbit FIFO buffers, 1-ms steps for 60 s. It isolates a *fixed* score snapshot;
// it models neither TCP ACK contention nor the feedback of Schedule over time.
func fixedShareQueue(shares map[int]int) (mbps, p95ms float64) {
	var queues [2]float64
	var delays []float64
	for tick := 0; tick < 60000; tick++ {
		for i := range queues {
			queues[i] = math.Min(50, queues[i]+.18*float64(shares[i+1])/100)
			sent := math.Min(.1, queues[i])
			queues[i] -= sent
			mbps += sent / 60
			delays = append(delays, queues[i]/100*1000)
		}
	}
	sort.Float64s(delays)
	return mbps, delays[len(delays)*95/100]
}

func TestCompareDirectionalHeadroom(t *testing.T) {
	a, b := loaded(1, 50), loaded(2, 50)
	a.TXMbps, b.TXMbps = 9.5, .1
	oldA, oldB := legacyScore(a), legacyScore(b)
	before := roundShares(map[int]float64{1: 100 * oldA / (oldA + oldB), 2: 100 * oldB / (oldA + oldB)}, 1)
	after := New(Params{}).Step([]Input{a, b}).Shares
	oldRate, oldDelay := fixedShareQueue(before)
	rate, delay := fixedShareQueue(after)
	t.Logf("MODEL ONLY fixed shares legacy=%v optimized=%v; served %.2f -> %.2f Mbit/s; queue p95 %.2f -> %.2f ms", before, after, oldRate, rate, oldDelay, delay)
	if rate < 179.9 || rate <= oldRate || delay >= oldDelay {
		t.Fatal("directional headroom regression")
	}
}

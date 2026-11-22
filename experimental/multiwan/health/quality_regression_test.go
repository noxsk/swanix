package health

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

func TestQualityRingAgainstRounds(t *testing.T) {
	for _, window := range []int{1, 3, 20} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			q := NewQuality(window)
			rng := rand.New(rand.NewSource(42))
			var history []RoundResult
			for round := 0; round < 1000; round++ {
				r := RoundResult{}
				for i, count := 0, rng.Intn(12); i < count; i++ {
					r.Targets = append(r.Targets, TargetResult{OK: rng.Intn(3) != 0, RTT: time.Duration(1+rng.Intn(100)) * time.Millisecond})
				}
				q.Push(r)
				history = append(history, r)
				if len(history) > window {
					history = history[1:]
				}
				total, bad := 0, 0
				var base time.Duration
				for _, h := range history {
					var rtts []time.Duration
					for _, target := range h.Targets {
						total++
						if target.OK {
							rtts = append(rtts, target.RTT)
						} else {
							bad++
						}
					}
					slices.Sort(rtts)
					if len(rtts) > 0 {
						median := rtts[len(rtts)/2]
						if base == 0 || median < base {
							base = median
						}
					}
				}
				loss := 0.0
				if total > 0 {
					loss = float64(bad) / float64(total)
				}
				if q.BaseRTT() != base || q.LossRate() != loss || q.Lossy() != (total >= 8 && loss > 0.3) {
					t.Fatalf("round %d: base=%v want %v loss=%v want %v lossy=%v total=%d", round, q.BaseRTT(), base, q.LossRate(), loss, q.Lossy(), total)
				}
			}
		})
	}
}

func TestQualityMinimumSamplesAndEmptyExpiry(t *testing.T) {
	q := NewQuality(2)
	q.Push(RoundResult{Targets: make([]TargetResult, 7)})
	if q.Lossy() {
		t.Fatal("seven probes must not be lossy")
	}
	q.Push(RoundResult{Targets: []TargetResult{{OK: true, RTT: 10 * time.Millisecond}}})
	if !q.Lossy() {
		t.Fatal("eight probes should enable loss detection")
	}
	q.Push(RoundResult{})
	if q.LossRate() != 0 || q.Lossy() {
		t.Fatal("failed targets did not expire")
	}
	q.Push(RoundResult{})
	if q.BaseRTT() != 0 {
		t.Fatal("empty rounds must expire baseline")
	}
	for _, window := range []int{0, -1} {
		q := NewQuality(window)
		q.Push(RoundResult{Targets: []TargetResult{{OK: true, RTT: time.Millisecond}}})
		q.Push(RoundResult{})
		if q.BaseRTT() != 0 {
			t.Fatal("nonpositive window should retain one round")
		}
	}
}

func TestDegradedRecoveryRequiresConsecutiveNormalRounds(t *testing.T) {
	m, now := newTest()
	step(m, now, true, false)
	step(m, now, true, true)
	step(m, now, true, true)
	for _, sample := range []struct{ ok, impaired bool }{
		{true, false}, {true, true}, {true, false}, {false, false}, {true, false},
	} {
		if tr := step(m, now, sample.ok, sample.impaired); tr != nil || m.State() != Degraded {
			t.Fatal("premature recovery", tr, m.State())
		}
	}
	if tr := step(m, now, true, false); tr == nil || m.State() != Healthy {
		t.Fatal("two normal rounds must restore quality")
	}
	step(m, now, true, true)
	step(m, now, true, true)
	for i := 0; i < 3; i++ {
		step(m, now, false, false)
	}
	if m.State() != Down {
		t.Fatal("degraded must retain Down threshold")
	}
	step(m, now, true, true)
	step(m, now, true, true)
	if m.State() != Recovering {
		t.Fatal("quality must not block recovery ramp")
	}
	step(m, now, false, false)
	if m.State() != Down {
		t.Fatal("recovery failure must immediately go Down")
	}
}

// legacyQuality freezes the pre-fix append/trim and target-RTT baseline
// implementation for performance comparison, not as a correctness oracle.
type legacyQuality struct {
	window int
	oks    []bool
	rtts   []time.Duration
}

func (q *legacyQuality) Push(r RoundResult) {
	for _, t := range r.Targets {
		q.oks = append(q.oks, t.OK)
		if t.OK {
			q.rtts = append(q.rtts, t.RTT)
		}
	}
	if n := len(q.oks) - q.window*4; n > 0 {
		q.oks = q.oks[n:]
	}
	if n := len(q.rtts) - q.window; n > 0 {
		q.rtts = q.rtts[n:]
	}
}

func (q *legacyQuality) Impaired(rtt time.Duration, loaded bool) bool {
	if len(q.oks) >= 8 {
		bad := 0
		for _, ok := range q.oks {
			if !ok {
				bad++
			}
		}
		if float64(bad)/float64(len(q.oks)) > 0.3 {
			return true
		}
	}
	if loaded {
		return false
	}
	var base time.Duration
	for _, r := range q.rtts {
		if base == 0 || r < base {
			base = r
		}
	}
	return base > 0 && rtt > 3*base+50*time.Millisecond
}

var benchmarkQualityImpaired bool

func BenchmarkQualityRing(b *testing.B) {
	for _, window := range []int{20, 2000} {
		for _, implementation := range []string{"legacy", "optimized"} {
			b.Run(fmt.Sprintf("%d/%s", window, implementation), func(b *testing.B) {
				var q interface {
					Push(RoundResult)
					Impaired(time.Duration, bool) bool
				}
				if implementation == "legacy" {
					q = &legacyQuality{window: window}
				} else {
					q = NewQuality(window)
				}
				r := RoundResult{Targets: []TargetResult{{OK: true, RTT: 5 * time.Millisecond}, {OK: true, RTT: 100 * time.Millisecond}, {OK: true, RTT: 110 * time.Millisecond}, {OK: false}}}
				// Measure steady-state full windows with identical probe inputs.
				for i := 0; i < window; i++ {
					q.Push(r)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					q.Push(r)
					benchmarkQualityImpaired = q.Impaired(100*time.Millisecond, false)
				}
			})
		}
	}
}

func TestQualityStateChurnComparison(t *testing.T) {
	// Explicitly simplified legacy reference: all rounds meet quorum, so
	// only Healthy/Degraded apply. Two impaired rounds degrade; one normal
	// round restores health. Down/Recovering are covered separately.
	legacyState, legacyBad := Healthy, 0
	legacySwitches, optimizedSwitches := 0, 0
	m, now := newTest()
	step(m, now, true, false)
	const cycles = 100
	for i := 0; i < cycles*3; i++ {
		bad := i%3 != 2 // bad, bad, good; "bad" means impaired, not failed quorum.
		before := legacyState
		if legacyState == Healthy {
			if bad {
				legacyBad++
			} else {
				legacyBad = 0
			}
			if legacyBad >= 2 {
				legacyState = Degraded
				legacyBad = 0
			}
		} else if !bad {
			legacyState = Healthy
		}
		if legacyState != before {
			legacySwitches++
		}
		if tr := step(m, now, true, bad); tr != nil {
			if !((tr.From == Healthy && tr.To == Degraded) || (tr.From == Degraded && tr.To == Healthy)) {
				t.Fatalf("unexpected transition: %+v", tr)
			}
			optimizedSwitches++
		}
	}
	t.Logf("bad bad good × %d: Healthy/Degraded switches legacy=%d optimized=%d; final legacy=%s optimized=%s", cycles, legacySwitches, optimizedSwitches, legacyState, m.State())
	if legacySwitches != 2*cycles || optimizedSwitches != 1 || legacyState != Healthy || m.State() != Degraded {
		t.Fatal("unexpected deterministic churn comparison")
	}
}

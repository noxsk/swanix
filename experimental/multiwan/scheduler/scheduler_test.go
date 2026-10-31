package scheduler

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

func wan(id int, down, up float64) Input {
	return Input{ID: id, State: health.Healthy, Role: config.RoleActive, DownMbps: down, UpMbps: up,
		Preference: 100, Bias: config.BiasDownload}
}

func total(m map[int]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func TestFixedCapacityWeights(t *testing.T) {
	s := New(Params{})
	r := s.Step([]Input{wan(1, 500, 50), wan(2, 300, 30), wan(3, 200, 20)})
	if total(r.Shares) != 100 || r.Shares[1] != 50 || r.Shares[2] != 30 || r.Shares[3] != 20 {
		t.Fatalf("got %v", r.Shares)
	}
}

func TestStaleMetricsFallBackToCapacity(t *testing.T) {
	a, b := wan(1, 100, 10), wan(2, 100, 10)
	a.MetricsFresh, b.MetricsFresh = false, false
	a.RXMbps = 99 // ignored: metrics not fresh
	r := New(Params{}).Step([]Input{a, b})
	if r.Shares[1] != 50 {
		t.Fatalf("got %v", r.Shares)
	}
}

func TestDownIsImmediate(t *testing.T) {
	s := New(Params{})
	s.Step([]Input{wan(1, 100, 10), wan(2, 100, 10)})
	w2 := wan(2, 100, 10)
	w2.State = health.Down
	r := s.Step([]Input{wan(1, 100, 10), w2})
	if r.Shares[1] != 100 || len(r.Shares) != 1 || !r.Changed {
		t.Fatalf("got %v", r.Shares)
	}
}

func TestAllDownRejects(t *testing.T) {
	a := wan(1, 100, 10)
	a.State = health.Down
	r := New(Params{}).Step([]Input{a})
	if len(r.Shares) != 0 {
		t.Fatalf("got %v", r.Shares)
	}
}

func TestBackupOnlyWhenActivesFail(t *testing.T) {
	s := New(Params{})
	b := wan(2, 100, 10)
	b.Role = config.RoleBackup
	r := s.Step([]Input{wan(1, 100, 10), b})
	if r.Shares[1] != 100 || r.Shares[2] != 0 {
		t.Fatalf("got %v", r.Shares)
	}
	a := wan(1, 100, 10)
	a.State = health.Down
	r = s.Step([]Input{a, b})
	if r.Shares[2] != 100 {
		t.Fatalf("backup not activated: %v", r.Shares)
	}
}

func TestSaturationShiftsGraduallyWithoutOscillation(t *testing.T) {
	s := New(Params{})
	mk := func(rx float64) []Input {
		a, b := wan(1, 100, 100), wan(2, 100, 100)
		a.MetricsFresh, b.MetricsFresh = true, true
		a.RXMbps = rx
		return []Input{a, b}
	}
	s.Step(mk(0))
	prev := 50
	var history []int
	for i := 0; i < 40; i++ {
		r := s.Step(mk(95)) // wan1 saturated
		if d := prev - r.Shares[1]; d > 10 {
			t.Fatalf("step %d moved %d points, limit is 10", i, d)
		}
		history = append(history, r.Shares[1])
		prev = r.Shares[1]
	}
	for i := 1; i < len(history); i++ {
		if history[i] > history[i-1]+1 {
			t.Fatalf("oscillation upwards at %d: %v", i, history)
		}
	}
	if history[len(history)-1] >= 30 {
		t.Fatalf("saturated line still has %d%%: %v", history[len(history)-1], history)
	}
}

func TestDeadbandSuppressesNoise(t *testing.T) {
	s := New(Params{})
	a, b := wan(1, 100, 10), wan(2, 100, 10)
	a.MetricsFresh, b.MetricsFresh = true, true
	s.Step([]Input{a, b})
	a.RXMbps = 5 // tiny utilisation difference
	r := s.Step([]Input{a, b})
	if r.Changed {
		t.Fatalf("deadband should hold shares, got %v", r.Shares)
	}
}

func TestRecoveryRampsUp(t *testing.T) {
	s := New(Params{})
	w2 := wan(2, 100, 10)
	w2.State = health.Recovering
	w2.RecoverRamp = 30 * time.Second
	var last int
	for _, el := range []time.Duration{0, 10, 20, 30} {
		w2.RecoverElapsed = el * time.Second
		r := s.Step([]Input{wan(1, 100, 10), w2})
		if r.Shares[2] < last {
			t.Fatalf("recovery share decreased: %d -> %d", last, r.Shares[2])
		}
		last = r.Shares[2]
	}
	for i := 0; i < 30; i++ {
		w2.RecoverElapsed = 30 * time.Second
		w2.State = health.Healthy
		s.Step([]Input{wan(1, 100, 10), w2})
	}
	r := s.Step([]Input{wan(1, 100, 10), w2})
	if r.Shares[2] < 47 || r.Shares[2] > 53 { // within the 3-point deadband of 50
		t.Fatalf("expected convergence near 50, got %v", r.Shares)
	}
}

func TestMinShareForUsableLines(t *testing.T) {
	r := New(Params{}).Step([]Input{wan(1, 10000, 10), wan(2, 1, 1)})
	if r.Shares[2] < 1 || total(r.Shares) != 100 {
		t.Fatalf("got %v", r.Shares)
	}
}

func TestSumAlways100(t *testing.T) {
	s := New(Params{})
	for i := 0; i < 200; i++ {
		a, b, c := wan(1, 500, 50), wan(2, 300, 30), wan(3, 200, 20)
		for _, w := range []*Input{&a, &b, &c} {
			w.MetricsFresh = true
		}
		a.RXMbps = float64(i*7%500) * 1
		b.RXMbps = float64(i*13%300) * 1
		c.RTT, c.BaseRTT = time.Duration(i%80)*time.Millisecond, 20*time.Millisecond
		if i%37 == 5 {
			b.State = health.Down
		}
		r := s.Step([]Input{a, b, c})
		if total(r.Shares) != 100 {
			t.Fatalf("iteration %d: %v", i, r.Shares)
		}
	}
}

func loaded(id int, rx float64) Input {
	w := wan(id, 100, 10)
	w.MetricsFresh, w.RXMbps = true, rx
	return w
}

func TestSaturatedLineSpillsAtOnce(t *testing.T) {
	s := New(Params{Saturation: 0.9})
	if r := s.Step([]Input{loaded(1, 10), loaded(2, 10)}); r.Shares[1] != 50 {
		t.Fatalf("start: %v", r.Shares)
	}
	// wan1 reaches 95 % of its 100 Mbit/s: new connections leave it in one step, not 10 points per update
	r := s.Step([]Input{loaded(1, 95), loaded(2, 10)})
	if r.Shares[1] != 1 || r.Shares[2] != 99 || !r.Saturated[1] || !r.Changed {
		t.Fatalf("spill: %v saturated=%v", r.Shares, r.Saturated)
	}
	// still above threshold-10 %: stays spilled (hysteresis)
	if r := s.Step([]Input{loaded(1, 85), loaded(2, 10)}); r.Shares[1] != 1 {
		t.Fatalf("released too early: %v", r.Shares)
	}
	// below 80 %: released, and it ramps back instead of jumping
	r = s.Step([]Input{loaded(1, 75), loaded(2, 10)})
	if r.Saturated[1] || r.Shares[1] <= 1 || r.Shares[1] > 11 {
		t.Fatalf("release should ramp: %v", r.Shares)
	}
}

func TestSpillNeedsAnUnsaturatedLine(t *testing.T) {
	s := New(Params{Saturation: 0.9})
	r := s.Step([]Input{loaded(1, 95), loaded(2, 96)})
	if len(r.Saturated) != 0 || r.Shares[1] < 30 || r.Shares[2] < 30 {
		t.Fatalf("with every line saturated the normal weights apply: %v %v", r.Shares, r.Saturated)
	}
}

func TestSpillIgnoresStaleMetricsAndIsOptional(t *testing.T) {
	a, b := loaded(1, 95), loaded(2, 10)
	a.MetricsFresh = false
	if r := New(Params{Saturation: 0.9}).Step([]Input{a, b}); len(r.Saturated) != 0 {
		t.Fatalf("stale metrics must not saturate: %v", r.Saturated)
	}
	if r := New(Params{}).Step([]Input{loaded(1, 95), loaded(2, 10)}); len(r.Saturated) != 0 || r.Shares[1] <= 1 {
		t.Fatalf("spill must be off unless configured: %v", r.Shares)
	}
}

func TestBusyRTTDoesNotCutScore(t *testing.T) {
	w := wan(1, 120, 120)
	w.MetricsFresh = true
	w.RXMbps = 80
	w.BaseRTT = 12 * time.Millisecond
	w.RTT = 12 * time.Millisecond
	idle := Score(w)
	w.RTT = 200 * time.Millisecond
	if Score(w) != idle {
		t.Fatalf("queueing delay changed the score: idle %v busy-rtt %v", idle, Score(w))
	}
	w.RXMbps = 0
	if Score(w) >= idle {
		t.Fatalf("idle delay should lower the score, got %v vs %v", Score(w), idle)
	}
}

func TestUtilisationFollowsBias(t *testing.T) {
	w := Input{DownMbps: 100, UpMbps: 10, RXMbps: 20, TXMbps: 9}
	for bias, want := range map[string]float64{config.BiasDownload: 0.2, config.BiasUpload: 0.9, config.BiasBalanced: 0.9} {
		w.Bias = bias
		if got := Utilisation(w); got < want-1e-9 || got > want+1e-9 {
			t.Fatalf("%s: %v, want %v", bias, got, want)
		}
	}
}

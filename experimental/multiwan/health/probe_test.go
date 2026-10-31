package health

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	for _, bad := range []string{"example.com:443", "1.1.1.1", "http://example.com/", "[::1"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q should be rejected (needs IP literal)", bad)
		}
	}
	tg, err := ParseTarget("https://1.1.1.1/cdn-cgi/trace")
	if err != nil || tg.Family != 4 || tg.Addr != "1.1.1.1:443" || tg.URL == nil {
		t.Fatalf("%+v %v", tg, err)
	}
	tg, err = ParseTarget("[2001:4860:4860::8888]:443")
	if err != nil || tg.Family != 6 {
		t.Fatalf("%+v %v", tg, err)
	}
}

func TestTargetsForFamilyAndDefaults(t *testing.T) {
	ts, _ := TargetsFor([]string{"1.1.1.1:443", "[2001:4860:4860::8888]:443"}, 6)
	if len(ts) != 1 || ts[0].Family != 6 {
		t.Fatal(ts)
	}
	ts, _ = TargetsFor([]string{"1.1.1.1:443"}, 6)
	if len(ts) != 3 {
		t.Fatalf("expected default v6 set, got %v", ts)
	}
}

type pipeConn struct{ net.Conn }

func TestRoundQuorumAndMark(t *testing.T) {
	var mu sync.Mutex
	var marks []uint32
	dial := func(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
		mu.Lock()
		marks = append(marks, mark)
		mu.Unlock()
		if addr == "9.9.9.9:443" {
			return nil, errors.New("unreachable")
		}
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	ts, _ := TargetsFor([]string{"1.1.1.1:443", "9.9.9.9:443", "8.8.8.8:443"}, 4)
	p := &Prober{Family: 4, Mark: 0x42000000, Targets: ts, Required: 2, Timeout: time.Second, Dial: dial}
	r := p.Round(context.Background())
	if !r.OK || r.Successes != 2 || r.Total != 3 {
		t.Fatalf("%+v", r)
	}
	for _, m := range marks {
		if m != 0x42000000 {
			t.Fatalf("probe not marked: %#x", m)
		}
	}
	p.Required = 3
	if p.Round(context.Background()).OK {
		t.Fatal("quorum of 3 must fail when one target is down")
	}
}

func TestSingleTargetFailureDoesNotDownWAN(t *testing.T) {
	fail := "1.1.1.1:443"
	dial := func(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
		if addr == fail {
			return nil, errors.New("x")
		}
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	ts, _ := TargetsFor(nil, 4)
	p := &Prober{Family: 4, Targets: ts, Required: 1, Timeout: time.Second, Dial: dial}
	now := time.Unix(0, 0)
	m := NewMachine(Params{DownRounds: 3, UpRounds: 2}, now)
	for i := 0; i < 10; i++ {
		m.Observe(now, p.Round(context.Background()).OK, false)
	}
	if m.State() != Healthy {
		t.Fatalf("one dead probe site took the line down: %v", m.State())
	}
}

func TestQualityDegraded(t *testing.T) {
	q := NewQuality(20)
	for i := 0; i < 10; i++ {
		q.Push(RoundResult{Targets: []TargetResult{{OK: true, RTT: 20 * time.Millisecond}}})
	}
	if q.Degraded(25 * time.Millisecond) {
		t.Fatal("normal latency flagged")
	}
	if !q.Degraded(400 * time.Millisecond) {
		t.Fatal("bufferbloat not flagged")
	}
	for i := 0; i < 10; i++ {
		q.Push(RoundResult{Targets: []TargetResult{{OK: false}, {OK: false}}})
	}
	if q.LossRate() < 0.3 || !q.Degraded(20*time.Millisecond) {
		t.Fatalf("loss %.2f", q.LossRate())
	}
}

func TestLoadedDelayIsNotImpairment(t *testing.T) {
	q := NewQuality(20)
	for i := 0; i < 10; i++ {
		q.Push(RoundResult{Targets: []TargetResult{{OK: true, RTT: 12 * time.Millisecond}}})
	}
	if !q.Slow(200 * time.Millisecond) {
		t.Fatal("idle delay should still be detected")
	}
	if q.Impaired(200*time.Millisecond, true) {
		t.Fatal("delay while forwarding is the queue, not a worse path")
	}
	if !q.Impaired(200*time.Millisecond, false) {
		t.Fatal("the same delay while idle is impairment")
	}
	if !QueueingDelay(120, 120, 80, 1) {
		t.Fatal("80 of 120 Mbps is load")
	}
	if QueueingDelay(120, 120, 1, 0.2) {
		t.Fatal("idle probe traffic is not load")
	}
}

package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

func TestInterfaceHistoryEvidenceAndOutages(t *testing.T) {
	r := newRig(t, true)
	if b, err := json.Marshal(r.e.InterfaceHistory()); err != nil || string(b) != `{"interval_seconds":30,"retention_seconds":21600,"points":[]}` {
		t.Fatalf("empty history: %s, %v", b, err)
	}
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	r.e.ProbeDue(context.Background())
	r.e.SampleInterfaces()
	first := r.e.InterfaceHistory().Points[0].Interfaces[0]
	if first.WAN != "wan1" || first.Family != 4 || first.RTTms == nil || first.ProbeFailureRate == nil || first.DownEvents != 0 {
		t.Fatalf("first sample lacks fresh probe evidence: %+v", first)
	}
	// A complete down/recovery cycle fits between the 30-second samples.
	ws := r.e.wans[0]
	fs := ws.fams[4]
	r.clk.advance(5 * time.Second)
	r.e.record(ws, 4, fs.m.Force(r.clk.now(), health.Down, "test outage"))
	r.clk.advance(5 * time.Second)
	r.e.record(ws, 4, fs.m.Force(r.clk.now(), health.Healthy, "test recovery"))
	r.e.Sample()
	if len(r.e.InterfaceHistory().Points) != 1 {
		t.Fatal("sampled before 30 seconds")
	}
	r.clk.advance(20 * time.Second)
	r.e.SampleInterfaces()
	second := r.e.InterfaceHistory().Points[1].Interfaces[0]
	if second.State != "healthy" || second.DownEvents != 1 || second.RTTms != nil || second.ProbeFailureRate != nil {
		t.Fatalf("short outage or stale evidence mishandled: %+v", second)
	}
	if *first.RTTms != *r.e.InterfaceHistory().Points[0].Interfaces[0].RTTms {
		t.Fatal("prior sample mutated")
	}
	// Failed probes retain their failure ratio but cannot reuse the old RTT.
	r.mu.Lock()
	r.failID[1] = true
	r.mu.Unlock()
	r.e.ProbeDue(context.Background())
	r.clk.advance(30 * time.Second)
	r.e.ProbeDue(context.Background())
	r.e.SampleInterfaces()
	third := r.e.InterfaceHistory().Points[2].Interfaces[0]
	if third.RTTms != nil || third.ProbeFailureRate == nil || *third.ProbeFailureRate <= 0 || third.DownEvents != 1 {
		t.Fatalf("failed round: %+v", third)
	}
	if first.ProbeFailureRate == nil || *first.ProbeFailureRate != 0 {
		t.Fatal("old failure ratio mutated")
	}
}

func TestInterfaceHistoryBoundedAndObserveMode(t *testing.T) {
	r := newRig(t, false)
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 725; i++ {
		r.e.SampleInterfaces()
		r.clk.advance(30 * time.Second)
	}
	h := r.e.InterfaceHistory()
	if len(h.Points) != 720 {
		t.Fatalf("history grew to %d points", len(h.Points))
	}
	for _, p := range h.Points {
		for _, s := range p.Interfaces {
			if s.Managing || s.RTTms != nil || s.ProbeFailureRate != nil {
				t.Fatalf("observe mode invented probes: %+v", s)
			}
		}
	}
	if h.Points[0].At.After(h.Points[len(h.Points)-1].At) {
		t.Fatal("history is not chronological")
	}
	// A suspended daemon must not return samples outside the six-hour window.
	r.clk.advance(7 * time.Hour)
	if len(r.e.InterfaceHistory().Points) != 0 {
		t.Fatal("returned history older than six hours")
	}
}

func TestInterfaceHistorySeparatesFamilies(t *testing.T) {
	r := newRig(t, true)
	r.e.o.Config.IPv6.Mode = config.IPv6Native
	e, err := New(r.e.o)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	e.ProbeDue(context.Background())
	e.SampleInterfaces()
	s := e.InterfaceHistory().Points[0].Interfaces
	if len(s) != 4 || s[0].Family != 4 || s[1].Family != 6 || s[0].RTTms == nil || s[1].RTTms != nil || s[1].State != "down" {
		t.Fatalf("IPv4 evidence leaked into unavailable IPv6: %+v", s)
	}
}

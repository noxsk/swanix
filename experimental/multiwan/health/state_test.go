package health

import (
	"testing"
	"time"
)

func newTest() (*Machine, *time.Time) {
	now := time.Unix(1000, 0)
	return NewMachine(Params{DownRounds: 3, UpRounds: 2, RecoverRamp: 30 * time.Second}, now), &now
}

func step(m *Machine, now *time.Time, ok, deg bool) *Transition {
	*now = now.Add(5 * time.Second)
	return m.Observe(*now, ok, deg)
}

func TestFlow(t *testing.T) {
	m, now := newTest()
	if tr := step(m, now, true, false); tr == nil || m.State() != Healthy {
		t.Fatal("first good round should be healthy")
	}
	// single failed round must not take the WAN down
	step(m, now, false, false)
	step(m, now, false, false)
	if m.State() != Healthy {
		t.Fatal("down_rounds not honoured")
	}
	step(m, now, true, false)
	step(m, now, false, false)
	step(m, now, false, false)
	if m.State() != Healthy {
		t.Fatal("failure streak must reset on success")
	}
	step(m, now, false, false)
	if m.State() != Down {
		t.Fatal("expected Down after 3 consecutive failures")
	}
	step(m, now, true, false)
	if m.State() != Down {
		t.Fatal("up_rounds not honoured")
	}
	step(m, now, true, false)
	if m.State() != Recovering {
		t.Fatalf("expected Recovering, got %v", m.State())
	}
	for i := 0; i < 8 && m.State() == Recovering; i++ {
		step(m, now, true, false)
	}
	if m.State() != Healthy {
		t.Fatalf("expected Healthy after ramp, got %v", m.State())
	}
}

func TestRecoveryFailureGoesDown(t *testing.T) {
	m, now := newTest()
	step(m, now, true, false)
	m.LinkEvent(*now, false, "link down")
	step(m, now, true, false)
	step(m, now, true, false)
	if m.State() != Recovering {
		t.Fatal(m.State())
	}
	step(m, now, false, false)
	if m.State() != Down {
		t.Fatal("any failure while recovering must go Down")
	}
}

func TestLinkEvents(t *testing.T) {
	m, now := newTest()
	step(m, now, true, false)
	if tr := m.LinkEvent(*now, false, "carrier lost"); tr == nil || m.State() != Down {
		t.Fatal("link down is immediate")
	}
	m.LinkEvent(*now, true, "")
	if m.State() != Down {
		t.Fatal("link up alone must not make WAN usable")
	}
}

func TestDegraded(t *testing.T) {
	m, now := newTest()
	step(m, now, true, false)
	step(m, now, true, true)
	if m.State() != Degraded {
		t.Fatal(m.State())
	}
	step(m, now, true, false)
	if m.State() != Healthy {
		t.Fatal(m.State())
	}
}

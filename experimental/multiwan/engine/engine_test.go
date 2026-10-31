package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
)

type fakeKernel struct {
	mu                                  sync.Mutex
	applies, runtimes, weights, removes int
	lastShares                          map[int]int
}

func (k *fakeKernel) Apply(in *kernel.Input) (*kernel.Ruleset, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.applies++
	return &kernel.Ruleset{}, nil
}
func (k *fakeKernel) UpdateRuntime(in *kernel.Input) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.runtimes++
	return nil
}
func (k *fakeKernel) UpdateWeights(in *kernel.Input) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.weights++
	k.lastShares = in.Shares["balanced"][4]
	return nil
}
func (k *fakeKernel) Counters(in *kernel.Input) (map[int]uint64, error) {
	return map[int]uint64{1: 7}, nil
}
func (k *fakeKernel) Remove(in *kernel.Input) error { k.removes++; return nil }

type fakeNetif struct {
	mu sync.Mutex
	up map[string]bool
}

func (f *fakeNetif) Status(name string) (netif.Interface, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "wan", "wan2":
		return netif.Interface{Name: name, Up: f.up[name], Available: true, Device: map[string]string{"wan": "eth1", "wan2": "eth2"}[name],
			Addrs4:   []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")},
			Gateway4: netip.MustParseAddr("10.0.0.1")}, true, nil
	case "lan":
		return netif.Interface{Name: "lan", Up: true, Available: true, Device: "br-lan",
			Addrs4: []netip.Prefix{netip.MustParsePrefix("192.168.1.1/24")}}, true, nil
	}
	return netif.Interface{}, false, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type rig struct {
	e   *Engine
	k   *fakeKernel
	nf  *fakeNetif
	clk *clock
	// failing marks addresses (by WAN probe mark) whose dials fail
	mu     sync.Mutex
	failID map[int]bool
}

func newRig(t *testing.T, manage bool) *rig {
	t.Helper()
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.Enabled = true
	cfg.IPv6.Mode = config.IPv6Unmanaged
	if manage {
		cfg.Global.StartupMode = config.StartupManage
	}
	for i := range cfg.Health {
		cfg.Health[i].DownRounds, cfg.Health[i].UpRounds, cfg.Health[i].RecoverRamp = 2, 2, 20
	}
	if err := cfg.Validate(config.ProfileLite); err != nil {
		t.Fatal(err)
	}
	r := &rig{k: &fakeKernel{}, nf: &fakeNetif{up: map[string]bool{"wan": true, "wan2": true}}, clk: &clock{t: time.Unix(1_000_000, 0)}, failID: map[int]bool{}}
	lay, _ := kernel.NewLayout(cfg.Kernel.MarkMask)
	dial := func(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.failID[lay.IDOf(mark)] {
			return nil, errors.New("timeout")
		}
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	e, err := New(Options{Config: cfg, Profile: config.ProfileLite, Version: "test", Netif: r.nf, Kernel: r.k, Dial: dial,
		Now: r.clk.now, Counters: func(string) (uint64, uint64, bool) { return 0, 0, false }})
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

// TestProbeTransitionIsReported guards the immediate reschedule: ProbeDue must say when a line changed state.
func TestProbeTransitionIsReported(t *testing.T) {
	r := newRig(t, true)
	_ = r.e.Reconcile()
	r.clk.advance(6 * time.Second)
	if !r.e.ProbeDue(context.Background()) {
		t.Fatal("first successful probe moves a line from unknown to healthy; that is a transition")
	}
	r.clk.advance(6 * time.Second)
	if r.e.ProbeDue(context.Background()) {
		t.Fatal("a steady state must not report a transition")
	}
	r.mu.Lock()
	r.failID[2] = true
	r.mu.Unlock()
	reported := false
	for i := 0; i < 3 && !reported; i++ {
		r.clk.advance(6 * time.Second)
		reported = r.e.ProbeDue(context.Background())
	}
	if !reported {
		t.Fatal("a line going down was not reported")
	}
}

// tick advances time and runs one probe pass plus a scheduling pass.
func (r *rig) tick(t *testing.T, d time.Duration) {
	t.Helper()
	r.clk.advance(d)
	r.e.ProbeDue(context.Background())
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) shares() map[int]int {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	out := map[int]int{}
	for k, v := range r.k.lastShares {
		out[k] = v
	}
	return out
}

func TestObserveModeWritesNothing(t *testing.T) {
	r := newRig(t, false)
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	r.tick(t, 10*time.Second)
	if r.k.applies+r.k.runtimes+r.k.weights != 0 {
		t.Fatalf("observe mode touched the kernel: %+v", r.k)
	}
	st := r.e.Status()
	if st.Managing || st.WANs[0].Families[0].State != "healthy" || st.WANs[0].Families[0].Reason == "" {
		t.Fatalf("observe status: %+v", st.WANs[0].Families[0])
	}
}

func TestFailoverAndRecovery(t *testing.T) {
	r := newRig(t, true)
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if r.k.applies != 1 {
		t.Fatalf("expected one full apply, got %d", r.k.applies)
	}
	for i := 0; i < 3; i++ {
		r.tick(t, 6*time.Second)
	}
	if s := r.shares(); s[1] < 30 || s[2] < 30 {
		t.Fatalf("healthy lines should share traffic: %v", s)
	}

	// wan2's probes start failing: after down_rounds it leaves the pool at once
	r.mu.Lock()
	r.failID[2] = true
	r.mu.Unlock()
	r.tick(t, 6*time.Second)
	r.tick(t, 6*time.Second)
	if s := r.shares(); s[1] != 100 || s[2] != 0 {
		t.Fatalf("wan2 should be out of the pool: %v", s)
	}
	if r.k.applies != 1 {
		t.Fatalf("health changes must not trigger a full apply (got %d)", r.k.applies)
	}
	w := r.k.weights

	// recovery: probes pass again, line re-enters gradually
	r.mu.Lock()
	r.failID[2] = false
	r.mu.Unlock()
	var firstShare int
	for i := 0; i < 20; i++ {
		r.tick(t, 6*time.Second)
		if s := r.shares()[2]; s > 0 && firstShare == 0 {
			firstShare = s
		}
	}
	if r.k.weights == w {
		t.Fatal("recovery never reached the kernel")
	}
	if firstShare == 0 || firstShare > 20 {
		t.Fatalf("recovering line must ramp from a small share, started at %d", firstShare)
	}
	if s := r.shares(); s[2] < 30 {
		t.Fatalf("line should be back near its fair share: %v", s)
	}
}

func TestLinkDownIsImmediateAndIncremental(t *testing.T) {
	r := newRig(t, true)
	_ = r.e.Reconcile()
	for i := 0; i < 3; i++ {
		r.tick(t, 6*time.Second)
	}
	r.nf.mu.Lock()
	r.nf.up["wan2"] = false
	r.nf.mu.Unlock()
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if s := r.shares(); s[1] != 100 {
		t.Fatalf("link down must drop wan2 immediately: %v", s)
	}
	if r.k.runtimes != 1 || r.k.applies != 1 {
		t.Fatalf("expected one incremental update, runtimes=%d applies=%d", r.k.runtimes, r.k.applies)
	}
}

func TestAllDownRejects(t *testing.T) {
	r := newRig(t, true)
	_ = r.e.Reconcile()
	for i := 0; i < 3; i++ {
		r.tick(t, 6*time.Second)
	}
	r.mu.Lock()
	r.failID[1], r.failID[2] = true, true
	r.mu.Unlock()
	r.tick(t, 6*time.Second)
	r.tick(t, 6*time.Second)
	if s := r.shares(); len(s) != 0 {
		t.Fatalf("no candidate must yield an empty pool (reject): %v", s)
	}
}

func TestStatus(t *testing.T) {
	r := newRig(t, true)
	_ = r.e.Reconcile()
	r.tick(t, 6*time.Second)
	st := r.e.Status()
	if len(st.WANs) != 2 || st.WANs[0].Assigned != 7 || st.Profile != "lite" {
		t.Fatalf("%+v", st)
	}
}

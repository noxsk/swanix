package engine

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
	"github.com/sagernet/sing-box/experimental/multiwan/wanhook"
)

// Frozen pre-snapshot implementation for apples-to-apples parallel benchmarks.
func legacyPick(e *Engine, policy string, family int) (wanhook.Selection, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.o.Config.PolicyByName(policy); !ok {
		return wanhook.Selection{}, fmt.Errorf("multiwan: unknown wan_policy %q", policy)
	}
	shares := e.shares[policy][family]
	total := 0
	for _, v := range shares {
		total += v
	}
	if total == 0 {
		return wanhook.Selection{}, fmt.Errorf("multiwan: no usable WAN for policy %q (ipv%d)", policy, family)
	}
	n := rand.IntN(total)
	ids := make([]int, 0, len(shares))
	for id := range shares {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if n < shares[id] {
			sel := wanhook.Selection{WAN: id, Mark: e.lay.WANMark(id)}
			if e.snap != nil && family == 4 {
				for _, w := range e.snap.WANs {
					if w.ID == id {
						sel.Src = w.Src4
					}
				}
			}
			return sel, nil
		}
		n -= shares[id]
	}
	return wanhook.Selection{}, fmt.Errorf("multiwan: scheduler state inconsistent")
}

func pickFixture(t testing.TB, count int) *Engine {
	t.Helper()
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.shares = kernel.Shares{"balanced": {4: {}, 6: {}}}
	e.snap = &netif.Snapshot{}
	for id := 1; id <= count; id++ {
		e.shares["balanced"][4][id] = id
		e.shares["balanced"][6][id] = id
		e.snap.WANs = append(e.snap.WANs, kernel.WANRuntime{ID: id, Src4: netip.AddrFrom4([4]byte{10, 0, 0, byte(id)})})
	}
	return e
}

func TestPickSnapshot(t *testing.T) {
	e := pickFixture(t, 4)
	// First Pick must honor pre-initialization direct assignments (existing benchmark).
	if _, err := e.Pick("balanced", 4); err != nil {
		t.Fatal(err)
	}
	old := e.picks.Load()
	for _, family := range []int{4, 6} {
		pool := old.policies["balanced"][family]
		counts := map[int]int{}
		for n := 0; n < pool.total; n++ {
			sel := pool.choose(n)
			counts[sel.WAN]++
			if sel.Mark != e.lay.WANMark(sel.WAN) {
				t.Fatal(sel)
			}
			if family == 4 && sel.Src != e.snap.WANs[sel.WAN-1].Src4 {
				t.Fatal(sel)
			}
			if family == 6 && sel.Src.IsValid() {
				t.Fatal(sel)
			}
		}
		for id := 1; id <= 4; id++ {
			if counts[id] != id {
				t.Fatal(counts)
			}
		}
	}
	for _, tc := range []struct {
		policy string
		family int
	}{{"missing", 4}, {"balanced", 5}} {
		_, want := legacyPick(e, tc.policy, tc.family)
		_, got := e.Pick(tc.policy, tc.family)
		if got == nil || got.Error() != want.Error() {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	e.mu.Lock()
	e.shares["balanced"][4] = map[int]int{1: 0}
	e.snap.WANs[0].Src4 = netip.MustParseAddr("192.0.2.1")
	e.publishPicks()
	e.mu.Unlock()
	if _, err := e.Pick("balanced", 4); err == nil {
		t.Fatal("empty pool accepted")
	}
	if old.policies["balanced"][4].choose(0).Src == e.snap.WANs[0].Src4 {
		t.Fatal("snapshot aliases runtime")
	}
	if old.policies["balanced"][4].total != 10 {
		t.Fatal("snapshot aliases shares")
	}
}

func TestPickReconcileAndSchedulePublication(t *testing.T) {
	r := newRig(t, false)
	// Reconcile must refresh selections even when weights have not changed.
	r.e.shares = kernel.Shares{"balanced": {4: {1: 100}}}
	if sel, err := r.e.Pick("balanced", 4); err != nil || sel.Src.IsValid() {
		t.Fatalf("before reconcile: %v, %v", sel, err)
	}
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if sel, err := r.e.Pick("balanced", 4); err != nil || sel.Src != netip.MustParseAddr("10.0.0.2") {
		t.Fatalf("after reconcile: %v, %v", sel, err)
	}
	r.nf.mu.Lock()
	r.nf.up["wan"], r.nf.up["wan2"] = false, false
	r.nf.mu.Unlock()
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Pick("balanced", 4); err == nil {
		t.Fatal("Schedule did not publish unavailable pool")
	}
}

func TestPickConcurrentControlPlane(t *testing.T) {
	r := newRig(t, false)
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if _, err := r.e.Pick("balanced", 4); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for j := 0; j < 100; j++ {
		if err := r.e.Reconcile(); err != nil {
			t.Fatal(err)
		}
		if err := r.e.Schedule(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	// Model a blocked kernel command holding the control-plane mutex.
	r.e.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := r.e.Pick("balanced", 4); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("Pick blocked on control-plane mutex")
	}
	r.e.mu.Unlock()
}

// Select WAN counts with -bench='BenchmarkPickComparison/WANs=4/' and
// CPUs with -cpu=1,2,4,8. Repeat with -count=5 for benchstat.
func BenchmarkPickComparison(b *testing.B) {
	for _, count := range []int{2, 4, 14} {
		b.Run(fmt.Sprintf("WANs=%d", count), func(b *testing.B) {
			for _, mode := range []string{"legacy", "optimized"} {
				b.Run(mode, func(b *testing.B) {
					e := pickFixture(b, count)
					e.publishPicks()
					pick := e.Pick
					if mode == "legacy" {
						pick = func(p string, f int) (wanhook.Selection, error) { return legacyPick(e, p, f) }
					}
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if _, err := pick("balanced", 4); err != nil {
								b.Error(err)
								return
							}
						}
					})
				})
			}
		})
	}
}

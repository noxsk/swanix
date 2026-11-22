package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/scheduler"
)

func runtimeObjectiveRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, true)
	r.e.o.Config.Policies = []config.Policy{{Name: "runtime", Members: []string{"wan1", "wan2"}, Objective: config.ObjectiveLatency}}
	r.e.o.Config.Global.SampleInterval = 2
	r.e.o.Config.Global.UpdateInterval = 1
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	r.e.ProbeDue(context.Background())
	for i, ws := range r.e.wans {
		ws.w.TrafficBias = config.BiasDownload
		ws.w.DownloadMbps = 100
		ws.sampledAt = r.clk.now()
		ws.h.Interval, ws.h.Timeout = 30, 2
		fs := ws.fams[4]
		fs.m.Force(r.clk.now(), health.Healthy, "test")
		fs.rtt = []time.Duration{10 * time.Millisecond, 100 * time.Millisecond}[i]
		fs.q = health.NewQuality(60)
		fs.q.Push(health.RoundResult{Targets: []health.TargetResult{{OK: true, RTT: fs.rtt}}})
		fs.objectiveQuality = health.NewQuality(60)
		fs.objectiveQuality.Push(health.RoundResult{OK: true, Targets: []health.TargetResult{{OK: true, RTT: fs.rtt}}})
	}
	return r
}

func TestObjectiveRuntimeBaselineAfterPartialRound(t *testing.T) {
	for _, required := range []int{2, 1} {
		t.Run(fmt.Sprintf("required=%d", required), func(t *testing.T) {
			r := runtimeObjectiveRig(t)
			r.e.o.Config.Policies[0].Objective = config.ObjectiveThroughput
			r.e.o.Config.Policies[0].LatencyReservePercent = 50
			partial := true
			lowMark := r.e.lay.ProbeMark(1)
			dial := func(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
				if mark != lowMark && partial {
					if addr != "1.1.1.1:443" {
						return nil, errors.New("target unavailable")
					}
				} else {
					delay := 10 * time.Millisecond
					if mark != lowMark {
						delay = 80 * time.Millisecond
					}
					time.Sleep(delay)
				}
				a, b := net.Pipe()
				_ = b.Close()
				return a, nil
			}
			r.e.o.Dial = dial
			for _, ws := range r.e.wans {
				fs := ws.fams[4]
				fs.q = health.NewQuality(60)
				fs.objectiveQuality = nil
				fs.lastSuccessTargets = ""
				fs.prober.Dial = dial
				fs.prober.Required = required
				fs.prober.Targets, _ = health.TargetsFor([]string{"1.1.1.1:443", "9.9.9.9:443"}, 4)
				fs.nextProbe = r.clk.now()
				// Avoid queueing-related health transitions obscuring the baseline test.
				ws.rx = 25
			}
			r.e.ProbeDue(context.Background())
			slow := r.e.wans[1].fams[4]
			oldBase := slow.q.BaseRTT()
			if oldBase <= 0 || slow.lastRoundOK != (required == 1) {
				t.Fatal("partial round did not exercise the expected quorum")
			}
			if required == 2 && slow.objectiveQuality != nil {
				t.Fatal("failed round populated objective baseline")
			}
			partial = false // ProbeDue joins every dial before returning.
			for _, ws := range r.e.wans {
				ws.fams[4].nextProbe = r.clk.now()
			}
			r.e.ProbeDue(context.Background())
			low := r.e.wans[0].fams[4]
			if slow.q.BaseRTT() != oldBase || slow.objectiveQuality.BaseRTT() != slow.rtt {
				t.Fatal("legacy baseline changed or objective retained partial-round RTT")
			}
			if slow.objectiveQuality.BaseRTT() <= low.objectiveQuality.BaseRTT()+5*time.Millisecond {
				t.Fatal("dial timing did not produce distinct baselines")
			}
			if err := r.e.Schedule(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(low.schedulingReasons["runtime"], "reserved") || strings.Contains(slow.schedulingReasons["runtime"], "reserved") {
				t.Fatalf("wrong reserve: low=%q slow=%q", low.schedulingReasons["runtime"], slow.schedulingReasons["runtime"])
			}
		})
	}
}

func TestObjectiveRuntimeFreshness(t *testing.T) {
	for _, tc := range []struct {
		name             string
		age              time.Duration
		loadAge          time.Duration
		failed, fallback bool
		missingBaseline  bool
	}{
		{name: "probe independent of update", age: 10 * time.Second},
		{name: "load independent of update", loadAge: 4 * time.Second},
		{name: "probe expiry boundary", age: 32 * time.Second, fallback: true},
		{name: "load expiry boundary", loadAge: 6 * time.Second, fallback: true},
		{name: "last round failed", failed: true, fallback: true},
		{name: "no objective baseline", missingBaseline: true, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeObjectiveRig(t)
			// A large update interval must not keep expired probe/load evidence alive.
			if tc.fallback {
				r.e.o.Config.Global.UpdateInterval = 100
			}
			for _, ws := range r.e.wans {
				ws.sampledAt = r.clk.now().Add(-tc.loadAge)
				fs := ws.fams[4]
				fs.probedAt = r.clk.now().Add(-tc.age)
				fs.lastRoundAt = fs.probedAt
				fs.lastRoundOK = !tc.failed
				if tc.missingBaseline {
					fs.objectiveQuality = nil
				}
			}
			if err := r.e.Schedule(); err != nil {
				t.Fatal(err)
			}
			share := r.e.shares["runtime"][4][1]
			if tc.fallback && share != 50 || !tc.fallback && share < 95 {
				t.Fatalf("share=%d fallback=%v", share, tc.fallback)
			}
		})
	}
}

func TestObjectiveRuntimeFailedProbeFallsBack(t *testing.T) {
	r := runtimeObjectiveRig(t)
	old := r.e.wans[1].fams[4].lastSuccessTargets
	r.failID[2] = true
	for _, ws := range r.e.wans {
		ws.fams[4].nextProbe = r.clk.now()
	}
	r.e.ProbeDue(context.Background())
	fs := r.e.wans[1].fams[4]
	if fs.lastRoundOK || !fs.m.State().Usable() || fs.lastSuccessTargets != old {
		t.Fatal("failed round must preserve signature but invalidate evidence")
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if r.e.shares["runtime"][4][1] != 50 {
		t.Fatal(r.e.shares)
	}
}

func TestObjectiveRuntimeTargetComparability(t *testing.T) {
	for _, kind := range []string{"configured", "successful", "unprobed", "reordered", "standby", "down", "backup pool"} {
		t.Run(kind, func(t *testing.T) {
			r := runtimeObjectiveRig(t)
			fs := r.e.wans[1].fams[4]
			in := []scheduler.Input{{ID: 1, State: health.Healthy, MetricsFresh: true}, {ID: 2, State: health.Healthy, MetricsFresh: true}}
			fallback := true
			switch kind {
			case "configured":
				fs.prober.Targets = append(fs.prober.Targets, health.Target{Raw: "4.4.4.4:443"})
			case "successful":
				fs.lastSuccessTargets = targetSignature([]string{"1.1.1.1:443"})
			case "unprobed":
				fs.lastSuccessTargets = ""
			case "reordered":
				targets := fs.prober.Targets
				for i, j := 0, len(targets)-1; i < j; i, j = i+1, j-1 {
					targets[i], targets[j] = targets[j], targets[i]
				}
				fallback = false
			case "standby":
				in[1].Role = config.RoleBackup
				fs.lastSuccessTargets = ""
				fallback = false
			case "down":
				in[1].State = health.Down
				fs.lastSuccessTargets = ""
				fallback = false
			case "backup pool":
				in[0].Role = config.RoleBackup
				in[1].Role = config.RoleBackup
				fs.lastSuccessTargets = "different"
			}
			r.e.requireComparableTargets(in, 4)
			if in[0].MetricsFresh == fallback {
				t.Fatalf("inputs=%+v", in)
			}
			if kind == "configured" || kind == "successful" || kind == "unprobed" {
				if err := r.e.Schedule(); err != nil {
					t.Fatal(err)
				}
				if r.e.shares["runtime"][4][1] != 50 {
					t.Fatal(r.e.shares)
				}
			}
		})
	}
	if targetSignature([]string{"b", "a"}) != targetSignature([]string{"a", "b"}) {
		t.Fatal("order dependent signature")
	}
}

type runtimeRetryKernel struct {
	Kernel
	updates   int
	failApply bool
	committed kernel.Shares
}

func (k *runtimeRetryKernel) UpdateWeights(in *kernel.Input) error {
	k.updates++
	if k.updates == 1 {
		return errors.New("commit failed")
	}
	k.committed = in.Shares
	return nil
}
func (k *runtimeRetryKernel) Apply(in *kernel.Input) (*kernel.Ruleset, error) {
	if k.failApply {
		return nil, errors.New("apply failed")
	}
	k.committed = in.Shares
	return k.Kernel.Apply(in)
}

func TestObjectiveRuntimeWeightCommitRetry(t *testing.T) {
	r := runtimeObjectiveRig(t)
	k := &runtimeRetryKernel{Kernel: r.k}
	r.e.o.Kernel = k
	if r.e.Schedule() == nil || !r.e.weightsDirty {
		t.Fatal("failed update not retained")
	}
	before := r.e.shares["runtime"][4]
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if k.updates != 2 || r.e.weightsDirty || !reflect.DeepEqual(before, k.committed["runtime"][4]) {
		t.Fatal("unchanged weights not retried")
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if k.updates != 2 {
		t.Fatal("clean weights resubmitted")
	}
}

func TestObjectiveRuntimeApplyCommitsDirtyWeights(t *testing.T) {
	r := runtimeObjectiveRig(t)
	r.e.applied = false
	k := &runtimeRetryKernel{Kernel: r.k, failApply: true}
	r.e.o.Kernel = k
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if !r.e.weightsDirty || k.updates != 0 {
		t.Fatal("pre-apply scheduling")
	}
	if r.e.Reconcile() == nil || !r.e.weightsDirty {
		t.Fatal("failed apply cleared dirty")
	}
	k.failApply = false
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if r.e.weightsDirty || !reflect.DeepEqual(r.e.shares, k.committed) {
		t.Fatal("apply did not commit current shares")
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if k.updates != 0 {
		t.Fatal("successful apply unnecessarily retried weights")
	}
}

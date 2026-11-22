// Package engine ties the MultiWAN pieces together: interface state, probes,
// scheduling and kernel updates. It depends only on the MultiWAN packages.
package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
	"github.com/sagernet/sing-box/experimental/multiwan/scheduler"
	"github.com/sagernet/sing-box/experimental/multiwan/telemetry"
	"github.com/sagernet/sing-box/experimental/multiwan/wanhook"
)

// Kernel is what the engine needs from the data path.
type Kernel interface {
	Apply(*kernel.Input) (*kernel.Ruleset, error)
	UpdateRuntime(*kernel.Input) error
	UpdateWeights(*kernel.Input) error
	Counters(*kernel.Input) (map[int]uint64, error)
	Remove(*kernel.Input) error
}

// ByteCounters returns cumulative rx/tx bytes of a device.
type ByteCounters func(dev string) (rx, tx uint64, ok bool)

// SysfsCounters reads /sys/class/net/<dev>/statistics.
func SysfsCounters(dev string) (uint64, uint64, bool) {
	read := func(name string) (uint64, bool) {
		b, err := os.ReadFile("/sys/class/net/" + dev + "/statistics/" + name)
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil
	}
	rx, ok1 := read("rx_bytes")
	tx, ok2 := read("tx_bytes")
	return rx, tx, ok1 && ok2
}

// Options configures an Engine.
type Options struct {
	Config   *config.Config
	Profile  config.BuildProfile
	Version  string
	Netif    netif.Source
	Kernel   Kernel
	Dial     health.DialFunc
	Counters ByteCounters
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

type famState struct {
	fam                int
	m                  *health.Machine
	q                  *health.Quality
	objectiveQuality   *health.Quality // successful rounds with the same successful target set
	prober             *health.Prober
	rtt                time.Duration
	linkUp             bool
	nextProbe          time.Time
	probedAt           time.Time
	lastRoundAt        time.Time
	lastRoundOK        bool
	lastSuccessTargets string // sorted successful Target values from the last successful round
	downEvents         uint64
	shares             map[string]int
	schedulingReasons  map[string]string
	saturated          map[string]bool // policy -> spilling over to unsaturated lines
}

type wanState struct {
	w              config.WAN
	h              config.Health
	fams           map[int]*famState
	dev            string
	rx, tx         float64 // smoothed Mbps
	instRX, instTX float64 // last sample interval, unsmoothed
	lastRX         uint64
	lastTX         uint64
	lastAt         time.Time
	sampledAt      time.Time
	haveLast       bool
	assigned       uint64
}

// Engine is safe for concurrent use.
type Engine struct {
	o   Options
	lay kernel.Layout

	mu               sync.Mutex
	picks            atomic.Pointer[pickSnapshot]
	wans             []*wanState
	scheds           map[string]*scheduler.Scheduler
	shares           kernel.Shares
	snap             *netif.Snapshot
	lastRuntime      string
	applied          bool
	weightsDirty     bool
	transitions      []telemetry.Transition
	history          *telemetry.Ring[telemetry.HistoryPoint]
	lastHist         time.Time
	interfaceHistory *telemetry.Ring[telemetry.InterfaceHistoryPoint]
	rejected         uint64
	reconcile        chan struct{}
}

// New builds an engine. The configuration must already be validated.
func New(o Options) (*Engine, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Counters == nil {
		o.Counters = SysfsCounters
	}
	lay, err := kernel.NewLayout(o.Config.Kernel.MarkMask)
	if err != nil {
		return nil, err
	}
	e := &Engine{o: o, lay: lay, scheds: map[string]*scheduler.Scheduler{}, shares: kernel.Shares{},
		history:          telemetry.NewRing[telemetry.HistoryPoint](720),
		interfaceHistory: telemetry.NewRing[telemetry.InterfaceHistoryPoint](720), reconcile: make(chan struct{}, 1)}
	now := o.Now()
	for _, w := range o.Config.WANs {
		if !w.Enabled {
			continue
		}
		h := o.Config.HealthFor(w)
		ws := &wanState{w: w, h: h, fams: map[int]*famState{}}
		for _, fam := range e.families() {
			targets, err := health.TargetsFor(append(append([]string(nil), h.Targets...), w.ExtraTargets...), fam)
			if err != nil {
				return nil, fmt.Errorf("wan.%s: %w", w.Name, err)
			}
			fs := &famState{fam: fam, shares: map[string]int{}}
			fs.m = health.NewMachine(health.Params{DownRounds: h.DownRounds, UpRounds: h.UpRounds,
				RecoverRamp: time.Duration(h.RecoverRamp) * time.Second}, now)
			fs.q = health.NewQuality(60)
			fs.prober = &health.Prober{Family: fam, Mark: lay.ProbeMark(w.ID), Targets: targets, Required: h.SuccessesRequired,
				Timeout: time.Duration(h.Timeout) * time.Second, Dial: o.Dial}
			ws.fams[fam] = fs
		}
		e.wans = append(e.wans, ws)
	}
	return e, nil
}

func (e *Engine) families() []int {
	if e.o.Config.IPv6.Mode == config.IPv6Unmanaged {
		return []int{4}
	}
	return []int{4, 6}
}

func (e *Engine) managing() bool {
	return e.o.Config.Global.Enabled && e.o.Config.Global.StartupMode == config.StartupManage
}

// Poke asks the engine to reconcile soon (hotplug, rpcd, netlink).
func (e *Engine) Poke() {
	select {
	case e.reconcile <- struct{}{}:
	default:
	}
}

func (e *Engine) record(ws *wanState, fam int, tr *health.Transition) {
	if tr == nil {
		return
	}
	if tr.To == health.Down && tr.From != health.Unknown {
		ws.fams[fam].downEvents++
	}
	e.transitions = append(e.transitions, telemetry.Transition{At: tr.At, WAN: ws.w.Name, Family: fam,
		From: tr.From.String(), To: tr.To.String(), Reason: tr.Reason})
	if len(e.transitions) > 50 {
		e.transitions = e.transitions[len(e.transitions)-50:]
	}
	e.o.Logf("wan %s ipv%d: %s -> %s (%s)", ws.w.Name, fam, tr.From, tr.To, tr.Reason)
	if tr.To == health.Down && e.o.Config.Proxy.InterruptOnDown {
		// proxy sockets on the dead line (Full): close them so their sessions redial elsewhere
		mark, name := e.lay.WANMark(ws.w.ID), ws.w.Name
		go func() {
			if n := wanhook.Interrupt(mark, fam); n > 0 {
				e.o.Logf("wan %s ipv%d: closed %d proxy connection(s) dialled through it", name, fam, n)
			}
		}()
	}
}

// Reconcile re-reads netifd, folds link changes into the state machines and
// updates the kernel: a full apply the first time, incremental afterwards.
func (e *Engine) Reconcile() error {
	snap, err := netif.Collect(e.o.Config, e.o.Netif)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.o.Now()
	e.snap = snap
	e.publishPicks()
	for _, ws := range e.wans {
		var rt kernel.WANRuntime
		for _, r := range snap.WANs {
			if r.ID == ws.w.ID {
				rt = r
			}
		}
		ws.dev = rt.Device4
		if ws.dev == "" {
			ws.dev = rt.Device6
		}
		for fam, fs := range ws.fams {
			up := rt.Up4
			reason := "IPv4 link down or no address"
			if fam == 6 {
				up = rt.Up6
				reason = "IPv6 unavailable (no interface, address or default route)"
			}
			if !ws.w.SupportsFamily(fam) {
				reason = "address family disabled for this line"
			}
			if up != fs.linkUp || fs.m.State() == health.Unknown && !up {
				fs.linkUp = up
				e.record(ws, fam, fs.m.LinkEvent(now, up, reason))
			}
			if !e.managing() && up && fs.m.State() != health.Healthy {
				// observe mode has no probe evidence, only netifd state
				e.record(ws, fam, fs.m.Force(now, health.Healthy, "observe: netifd link state only, not probed"))
			}
		}
	}
	if !e.managing() {
		return nil
	}
	in := e.input(snap)
	key := fmt.Sprintf("%+v|%+v", in.WANs, in.LAN)
	switch {
	case !e.applied:
		if _, err := e.o.Kernel.Apply(in); err != nil {
			return err
		}
		e.applied, e.lastRuntime = true, key
		// Apply includes the current shares; only a successful apply commits them.
		e.weightsDirty = false
	case key != e.lastRuntime:
		if err := e.o.Kernel.UpdateRuntime(in); err != nil {
			return err
		}
		e.lastRuntime = key
	}
	return nil
}

// input assembles the kernel input. Caller holds e.mu.
func (e *Engine) input(snap *netif.Snapshot) *kernel.Input {
	wans := append([]kernel.WANRuntime(nil), snap.WANs...)
	for i := range wans {
		for _, ws := range e.wans {
			if ws.w.ID == wans[i].ID {
				if fs := ws.fams[6]; fs != nil && fs.m.State() == health.Down {
					wans[i].Down6 = true
				}
			}
		}
	}
	return &kernel.Input{Cfg: e.o.Config, WANs: wans, LAN: snap.LAN, Shares: e.shares}
}

// deviceOf is the current device of a WAN for a family, from the last netifd snapshot.
func (e *Engine) deviceOf(id, fam int) string {
	if e.snap == nil {
		return ""
	}
	for _, w := range e.snap.WANs {
		if w.ID == id {
			if fam == 6 {
				return w.Device6
			}
			return w.Device4
		}
	}
	return ""
}

// ProbeDue runs every probe round that is due, in parallel across WANs. It reports whether
// any line changed state, so the caller can reschedule at once instead of at the next tick.
func (e *Engine) ProbeDue(ctx context.Context) (changed bool) {
	if !e.managing() || e.o.Dial == nil {
		return false
	}
	type job struct {
		ws *wanState
		fs *famState
	}
	var jobs []job
	e.mu.Lock()
	now := e.o.Now()
	for _, ws := range e.wans {
		for _, fs := range ws.fams {
			if fs.linkUp && !now.Before(fs.nextProbe) {
				fs.nextProbe = now.Add(time.Duration(ws.h.Interval) * time.Second)
				fs.prober.Device = e.deviceOf(ws.w.ID, fs.fam)
				jobs = append(jobs, job{ws, fs})
			}
		}
	}
	e.mu.Unlock()
	results := make([]health.RoundResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = j.fs.prober.Round(ctx)
		}()
	}
	wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	now = e.o.Now()
	for i, j := range jobs {
		r := results[i]
		j.fs.q.Push(r)
		j.fs.lastRoundAt, j.fs.lastRoundOK = now, r.OK
		if r.OK {
			j.fs.rtt = r.RTT
			j.fs.probedAt = now
			var targets []string
			for _, target := range r.Targets {
				if target.OK {
					targets = append(targets, target.Target)
				}
			}
			signature := targetSignature(targets)
			if j.fs.objectiveQuality == nil || signature != j.fs.lastSuccessTargets {
				j.fs.objectiveQuality = health.NewQuality(60)
			}
			j.fs.objectiveQuality.Push(r)
			j.fs.lastSuccessTargets = signature
		}
		degraded := r.OK && j.fs.q.Impaired(r.RTT, wsQueueing(j.ws))
		if tr := j.fs.m.Observe(now, r.OK, degraded); tr != nil {
			e.record(j.ws, j.fs.fam, tr)
			changed = true
		}
	}
	return changed
}

// Sample reads device byte counters and smooths the rates (EWMA alpha 0.2).
func (e *Engine) Sample() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.o.Now()
	for _, ws := range e.wans {
		if ws.dev == "" {
			continue
		}
		rx, tx, ok := e.o.Counters(ws.dev)
		if !ok {
			continue
		}
		if ws.haveLast {
			dt := now.Sub(ws.lastAt).Seconds()
			if dt > 0 && rx >= ws.lastRX && tx >= ws.lastTX {
				const alpha = 0.2
				ws.instRX = float64(rx-ws.lastRX) * 8 / dt / 1e6
				ws.instTX = float64(tx-ws.lastTX) * 8 / dt / 1e6
				ws.rx = alpha*ws.instRX + (1-alpha)*ws.rx
				ws.tx = alpha*ws.instTX + (1-alpha)*ws.tx
				ws.sampledAt = now
			}
		}
		ws.lastRX, ws.lastTX, ws.lastAt, ws.haveLast = rx, tx, now, true
	}
	if now.Sub(e.lastHist) >= 5*time.Second {
		e.lastHist = now
		pt := telemetry.HistoryPoint{At: now, RX: map[string]float64{}, TX: map[string]float64{}}
		for _, ws := range e.wans {
			pt.RX[ws.w.Name], pt.TX[ws.w.Name] = ws.rx, ws.tx
		}
		e.history.Push(pt)
	}
}

// SampleInterfaces records health independently of the throughput sample rate.
func (e *Engine) SampleInterfaces() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sampleInterfaces(e.o.Now())
}

// sampleInterfaces runs under e.mu. Probe metrics expire when their next
// expected round is overdue, so an old success cannot look like fresh evidence.
func (e *Engine) sampleInterfaces(now time.Time) {
	pt := telemetry.InterfaceHistoryPoint{At: now, Interfaces: []telemetry.InterfaceSample{}}
	for _, ws := range e.wans {
		for _, fam := range e.families() {
			fs := ws.fams[fam]
			s := telemetry.InterfaceSample{WAN: ws.w.Name, Network: ws.w.Network,
				Device: e.deviceOf(ws.w.ID, fam), Family: fam, State: fs.m.State().String(),
				Reason: fs.m.Reason(), Managing: e.managing(), DownEvents: fs.downEvents}
			fresh := !fs.lastRoundAt.IsZero() && now.Sub(fs.lastRoundAt) <= time.Duration(ws.h.Interval+ws.h.Timeout)*time.Second
			if e.managing() && fs.linkUp && fresh {
				failure := fs.q.LossRate()
				s.ProbeFailureRate = &failure
				if fs.lastRoundOK {
					rtt := float64(fs.rtt) / float64(time.Millisecond)
					s.RTTms = &rtt
				}
			}
			pt.Interfaces = append(pt.Interfaces, s)
			fs.downEvents = 0
		}
	}
	e.interfaceHistory.Push(pt)
}

// Schedule recomputes per-policy shares and, if they changed, refreshes the
// kernel weight maps with one atomic batch.
func (e *Engine) Schedule() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.o.Now()
	staleAfter := time.Duration(3*e.o.Config.Global.UpdateInterval) * time.Second
	changed := false
	for _, pol := range e.o.Config.Policies {
		for _, fam := range e.families() {
			if fam == 6 && e.o.Config.IPv6.Mode == config.IPv6Native {
				continue
			}
			var in []scheduler.Input
			for _, name := range pol.Members {
				ws := e.byName(name)
				if ws == nil {
					continue
				}
				fs := ws.fams[fam]
				rx, tx := ws.rate()
				si := scheduler.Input{ID: ws.w.ID, State: fs.m.State(), Role: ws.w.Role, DownMbps: ws.w.DownloadMbps,
					UpMbps: ws.w.UploadMbps, Preference: ws.w.Preference, Bias: ws.w.TrafficBias, RXMbps: rx, TXMbps: tx,
					BaseRTT: fs.q.BaseRTT(), RTT: fs.rtt, LossRate: fs.q.LossRate(),
					MetricsFresh:   !ws.sampledAt.IsZero() && now.Sub(ws.sampledAt) < staleAfter && !fs.probedAt.IsZero() && now.Sub(fs.probedAt) < staleAfter,
					RecoverElapsed: fs.m.RecoveryElapsed(now), RecoverRamp: time.Duration(ws.h.RecoverRamp) * time.Second}
				if pol.Objective != "" && pol.Objective != config.ObjectiveCapacity {
					si.BaseRTT = 0
					if fs.objectiveQuality != nil {
						si.BaseRTT = fs.objectiveQuality.BaseRTT()
					}
					si.MetricsFresh = !ws.sampledAt.IsZero() && now.Sub(ws.sampledAt) < time.Duration(3*e.o.Config.Global.SampleInterval)*time.Second &&
						!fs.lastRoundAt.IsZero() && fs.lastRoundOK && now.Sub(fs.lastRoundAt) < time.Duration(ws.h.Interval+ws.h.Timeout)*time.Second
				}
				in = append(in, si)
			}
			key := fmt.Sprintf("%s|%d", pol.Name, fam)
			s := e.scheds[key]
			if s == nil {
				s = scheduler.New(scheduler.ParamsForPolicy(pol))
				e.scheds[key] = s
			}
			if pol.Objective != "" && pol.Objective != config.ObjectiveCapacity {
				e.requireComparableTargets(in, fam)
			}
			res := s.Step(in)
			if e.shares[pol.Name] == nil {
				e.shares[pol.Name] = map[int]map[int]int{}
			}
			e.shares[pol.Name][fam] = res.Shares
			for _, name := range pol.Members {
				if ws := e.byName(name); ws != nil {
					fs := ws.fams[fam]
					fs.shares[pol.Name] = res.Shares[ws.w.ID]
					if fs.schedulingReasons == nil {
						fs.schedulingReasons = map[string]string{}
					}
					fs.schedulingReasons[pol.Name] = res.Reasons[ws.w.ID]
					if sat := res.Saturated[ws.w.ID]; sat != fs.saturated[pol.Name] {
						if fs.saturated == nil {
							fs.saturated = map[string]bool{}
						}
						fs.saturated[pol.Name] = sat
						if sat {
							e.o.Logf("wan %s ipv%d: saturated in policy %s (%.0f%% of its capacity): bounded score discount; gradual redistribution",
								name, fam, pol.Name, 100*scheduler.Utilisation(scheduler.Input{DownMbps: ws.w.DownloadMbps, UpMbps: ws.w.UploadMbps,
									Bias: ws.w.TrafficBias, RXMbps: ws.rx, TXMbps: ws.tx}))
						} else {
							e.o.Logf("wan %s ipv%d: no longer saturated in policy %s", name, fam, pol.Name)
						}
					}
				}
			}
			changed = changed || res.Changed
		}
	}
	e.publishPicks()
	e.weightsDirty = e.weightsDirty || changed
	if !e.weightsDirty || !e.managing() || !e.applied {
		return nil
	}
	if err := e.o.Kernel.UpdateWeights(e.input(e.snap)); err != nil {
		return err
	}
	e.weightsDirty = false
	return nil
}

// targetSignature is order independent and unambiguous even for URL targets.
func targetSignature(targets []string) string {
	sort.Strings(targets)
	var b strings.Builder
	for _, target := range targets {
		fmt.Fprintf(&b, "%d:%s", len(target), target)
	}
	return b.String()
}

// requireComparableTargets mirrors the scheduler's candidate selection: standby
// backups and unusable lines must not invalidate the active pool's evidence.
func (e *Engine) requireComparableTargets(in []scheduler.Input, fam int) {
	active := false
	for _, w := range in {
		active = active || w.State.Usable() && w.Role != config.RoleBackup
	}
	var configured, successful string
	first, comparable := true, true
	for _, w := range in {
		if !w.State.Usable() || (w.Role != config.RoleBackup) != active {
			continue
		}
		for _, ws := range e.wans {
			if ws.w.ID != w.ID {
				continue
			}
			fs := ws.fams[fam]
			var targets []string
			for _, target := range fs.prober.Targets {
				targets = append(targets, target.Raw)
			}
			sig := targetSignature(targets)
			if fs.lastSuccessTargets == "" || !first && (configured != sig || successful != fs.lastSuccessTargets) {
				comparable = false
			}
			configured, successful, first = sig, fs.lastSuccessTargets, false
		}
	}
	if !comparable {
		for i := range in {
			in[i].MetricsFresh = false
		}
	}
}

// rate is the faster of the smoothed rate and the last sample. The smoothed
// value lags a transfer by several intervals, which made a speed test look idle.
func (ws *wanState) rate() (rx, tx float64) {
	rx, tx = ws.rx, ws.tx
	if ws.instRX > rx {
		rx = ws.instRX
	}
	if ws.instTX > tx {
		tx = ws.instTX
	}
	return rx, tx
}

func wsQueueing(ws *wanState) bool {
	rx, tx := ws.rate()
	return health.QueueingDelay(ws.w.DownloadMbps, ws.w.UploadMbps, rx, tx)
}

func (e *Engine) byName(n string) *wanState {
	for _, ws := range e.wans {
		if ws.w.Name == n {
			return ws
		}
	}
	return nil
}

// Run drives the engine until ctx is done. The kernel state is deliberately
// left in place on exit; use Remove to withdraw it.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.Reconcile(); err != nil {
		e.o.Logf("initial reconcile: %v", err)
	}
	if err := e.Schedule(); err != nil {
		e.o.Logf("schedule: %v", err)
	}
	e.SampleInterfaces()
	g := e.o.Config.Global
	sample := time.NewTicker(time.Duration(g.SampleInterval) * time.Second)
	interfaceSample := time.NewTicker(30 * time.Second)
	update := time.NewTicker(time.Duration(g.UpdateInterval) * time.Second)
	probe := time.NewTicker(time.Second)
	reconcile := time.NewTicker(30 * time.Second) // compensates lost events
	defer sample.Stop()
	defer interfaceSample.Stop()
	defer update.Stop()
	defer probe.Stop()
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sample.C:
			e.Sample()
		case <-interfaceSample.C:
			e.SampleInterfaces()
		case <-probe.C:
			if e.ProbeDue(ctx) {
				// a line going down must leave the pool now, not at the next scheduling tick
				if err := e.Schedule(); err != nil {
					e.o.Logf("schedule: %v", err)
				}
			}
		case <-update.C:
			if err := e.Schedule(); err != nil {
				e.o.Logf("schedule: %v", err)
			}
		case <-reconcile.C:
			e.Poke()
		case <-e.reconcile:
			if err := e.Reconcile(); err != nil {
				e.o.Logf("reconcile: %v", err)
			}
			// a link change must reach the weights immediately, not at the next tick
			if err := e.Schedule(); err != nil {
				e.o.Logf("schedule: %v", err)
			}
		}
	}
}

// Remove withdraws everything swanix installed.
func (e *Engine) Remove() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	snap := e.snap
	if snap == nil {
		snap = &netif.Snapshot{}
	}
	e.applied = false
	return e.o.Kernel.Remove(e.input(snap))
}

// Status returns the current status document.
func (e *Engine) Status() telemetry.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.o.Now()
	st := telemetry.Status{Version: e.o.Version, Profile: e.o.Profile.Name, Enabled: e.o.Config.Global.Enabled,
		Mode: e.o.Config.Global.StartupMode, Managing: e.managing(), GeneratedAt: now,
		Transitions: append([]telemetry.Transition(nil), e.transitions...), Problems: []string{}}
	if e.snap != nil {
		st.Problems = append(st.Problems, e.snap.Problems...)
	}
	var counters map[int]uint64
	if e.managing() && e.applied && e.snap != nil {
		if c, err := e.o.Kernel.Counters(e.input(e.snap)); err == nil {
			counters = c
			st.Rejected = c[kernel.RejectID]
		}
	}
	for _, ws := range e.wans {
		w := telemetry.WANStatus{ID: ws.w.ID, Name: ws.w.Name, Network: ws.w.Network, Device: ws.dev,
			DownloadMbps: ws.w.DownloadMbps, UploadMbps: ws.w.UploadMbps, RXMbps: ws.rx, TXMbps: ws.tx, Assigned: counters[ws.w.ID]}
		for _, fam := range e.families() {
			fs := ws.fams[fam]
			f := telemetry.FamilyStatus{Family: fam, State: fs.m.State().String(), Reason: fs.m.Reason(), Since: fs.m.Since(),
				RTTms: float64(fs.rtt) / float64(time.Millisecond), BaseMs: float64(fs.q.BaseRTT()) / float64(time.Millisecond),
				ProbeFailureRate: fs.q.LossRate(), Share: map[string]int{}, SchedulingReason: map[string]string{}}
			for k, v := range fs.shares {
				f.Share[k] = v
			}
			for k, v := range fs.schedulingReasons {
				if v != "" {
					f.SchedulingReason[k] = v
				}
			}
			for k, sat := range fs.saturated {
				if sat {
					f.Saturated = append(f.Saturated, k)
				}
			}
			sort.Strings(f.Saturated)
			w.Families = append(w.Families, f)
		}
		st.WANs = append(st.WANs, w)
	}
	return st
}

// History returns the throughput ring (720 points of 5 s, about one hour).
func (e *Engine) History() []telemetry.HistoryPoint { return e.history.Snapshot() }

func (e *Engine) InterfaceHistory() telemetry.InterfaceHistory {
	points := e.interfaceHistory.Snapshot()
	cutoff := e.o.Now().Add(-6 * time.Hour)
	for len(points) > 0 && points[0].At.Before(cutoff) {
		points = points[1:]
	}
	if points == nil {
		points = []telemetry.InterfaceHistoryPoint{}
	}
	return telemetry.InterfaceHistory{IntervalSeconds: 30, RetentionSeconds: 6 * 60 * 60,
		Points: points}
}

// Pick implements wanhook.Selector for the Full build's dialers: a weighted
// random choice over the policy's current shares. With no candidate it fails,
// which is the "reject" behaviour of unavailable_action.
func (e *Engine) Pick(policy string, family int) (wanhook.Selection, error) {
	snap := e.picks.Load()
	if snap == nil {
		// Lazy initialization also supports callers seeding shares before first use.
		e.mu.Lock()
		snap = e.picks.Load()
		if snap == nil {
			e.publishPicks()
			snap = e.picks.Load()
		}
		e.mu.Unlock()
	}
	families, ok := snap.policies[policy]
	if !ok {
		return wanhook.Selection{}, fmt.Errorf("multiwan: unknown wan_policy %q", policy)
	}
	pool := families[family]
	if pool.total == 0 {
		return wanhook.Selection{}, fmt.Errorf("multiwan: no usable WAN for policy %q (ipv%d)", policy, family)
	}
	return pool.choose(rand.IntN(pool.total)), nil
}

// A published snapshot owns all its maps/slices and contains only value selections.
// Config is immutable after New, as with the rest of the engine.
type pickSnapshot struct {
	policies map[string]map[int]pickPool
}

type pickCandidate struct {
	end int
	sel wanhook.Selection
}

type pickPool struct {
	total      int
	candidates []pickCandidate
}

func (p pickPool) choose(n int) wanhook.Selection {
	for _, c := range p.candidates {
		if n < c.end {
			return c.sel
		}
	}
	panic("multiwan: scheduler state inconsistent")
}

// publishPicks runs under e.mu, before potentially blocking kernel operations.
// Like the old Pick, it uses the in-memory shares even if a kernel update fails.
func (e *Engine) publishPicks() {
	snap := &pickSnapshot{policies: make(map[string]map[int]pickPool, len(e.o.Config.Policies))}
	for _, policy := range e.o.Config.Policies {
		families := make(map[int]pickPool)
		for family, shares := range e.shares[policy.Name] {
			ids := make([]int, 0, len(shares))
			for id, weight := range shares {
				if weight > 0 {
					ids = append(ids, id)
				}
			}
			sort.Ints(ids)
			pool := pickPool{candidates: make([]pickCandidate, 0, len(ids))}
			for _, id := range ids {
				sel := wanhook.Selection{WAN: id, Mark: e.lay.WANMark(id)}
				if e.snap != nil && family == 4 {
					for _, w := range e.snap.WANs {
						if w.ID == id {
							sel.Src = w.Src4
						}
					}
				}
				pool.total += shares[id]
				pool.candidates = append(pool.candidates, pickCandidate{end: pool.total, sel: sel})
			}
			families[family] = pool
		}
		snap.policies[policy.Name] = families
	}
	e.picks.Store(snap)
}

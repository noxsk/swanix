// Package scheduler turns per-WAN health and load metrics into integer
// new-connection shares (percent, summing to 100). The score is an engineering
// heuristic, not an optimum, and it allocates connection opportunities, not
// bytes.
package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

// Params are the scheduler constraints. Zero values select the defaults.
type Params struct {
	// Objective is capacity (legacy), latency, throughput, or balanced.
	Objective string
	// FlowSizeKiB is a reference transfer size, never an inferred flow size.
	FlowSizeKiB int
	// LatencyReservePercent subtracts soft headroom on the lowest-baseline
	// lines for throughput policies. It does not shape existing connections.
	LatencyReservePercent int
	MinShare              float64 // percent for every usable candidate, default 1
	MaxStep               float64 // max percentage points per update, default 10
	Deadband              float64 // changes below this many points are ignored, default 3
	Alpha                 float64 // EWMA factor for target shares, default 0.2
	// Saturation (0 = off, else 0.5–0.99) is the utilisation of the configured
	// capacity at which a line counts as saturated. With fresh alternative
	// capacity, apply a bounded score discount, preserving objective factors,
	// EWMA and step limits. Saturation is not a failure or a hard 1% floor.
	// A line is released saturationRelease below the threshold.
	Saturation float64
}

// saturationRelease is the hysteresis below the threshold.
const saturationRelease = 0.10

// ParamsForPolicy keeps live scheduling and dry-run preview consistent.
func ParamsForPolicy(p config.Policy) Params {
	return Params{Objective: p.Objective, FlowSizeKiB: p.FlowSizeKiB,
		LatencyReservePercent: p.LatencyReservePercent,
		Saturation:            float64(p.SaturationThreshold) / 100}
}

func (p Params) withDefaults() Params {
	if p.Objective == "" {
		p.Objective = config.ObjectiveCapacity
	}
	if p.FlowSizeKiB <= 0 {
		p.FlowSizeKiB = 256
	}
	if p.MinShare == 0 {
		p.MinShare = 1
	}
	if p.MaxStep == 0 {
		p.MaxStep = 10
	}
	if p.Deadband == 0 {
		p.Deadband = 3
	}
	if p.Alpha == 0 {
		p.Alpha = 0.2
	}
	return p
}

// Input is a snapshot of one WAN for one address family.
type Input struct {
	ID             int
	State          health.State
	Role           string
	DownMbps       float64
	UpMbps         float64
	Preference     int
	Bias           string
	RXMbps, TXMbps float64 // smoothed actual rates
	BaseRTT        time.Duration
	RTT            time.Duration
	LossRate       float64 // probe failure ratio, not link packet loss
	MetricsFresh   bool
	RecoverElapsed time.Duration
	RecoverRamp    time.Duration
}

// Result is one scheduler decision.
type Result struct {
	Shares    map[int]int // WAN ID -> percent; only usable WANs present, sum 100
	Changed   bool        // false when the deadband kept the previous shares
	Scores    map[int]float64
	Reasons   map[int]string
	Saturated map[int]bool // saturation pressure with a fresh unsaturated alternative; not a hard floor
}

// Scheduler keeps the smoothing state between updates.
type Scheduler struct {
	p         Params
	smoothed  map[int]float64
	applied   map[int]float64
	cands     map[int]bool
	saturated map[int]bool // above the threshold (with hysteresis), whether or not spilling
}

func New(p Params) *Scheduler {
	return &Scheduler{p: p.withDefaults(), smoothed: map[int]float64{}, applied: map[int]float64{}, cands: map[int]bool{},
		saturated: map[int]bool{}}
}

// Utilisation is the used fraction of the configured capacity in the direction
// the line is biased to (the busier direction for balanced lines).
func Utilisation(in Input) float64 {
	down, up := 0.0, 0.0
	if in.DownMbps > 0 {
		down = in.RXMbps / in.DownMbps
	}
	if in.UpMbps > 0 {
		up = in.TXMbps / in.UpMbps
	}
	switch in.Bias {
	case config.BiasDownload:
		return down
	case config.BiasUpload:
		return up
	}
	return math.Max(down, up)
}

// updateSaturation applies the threshold with hysteresis; lines without fresh
// metrics are never considered saturated.
func (s *Scheduler) updateSaturation(cands []Input) {
	next := map[int]bool{}
	for _, w := range cands {
		if !w.MetricsFresh {
			continue
		}
		u := Utilisation(w)
		if u >= s.p.Saturation || s.saturated[w.ID] && u >= s.p.Saturation-saturationRelease {
			next[w.ID] = true
		}
	}
	s.saturated = next
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func capacity(in Input) float64 {
	switch in.Bias {
	case config.BiasDownload:
		return in.DownMbps
	case config.BiasUpload:
		return in.UpMbps
	}
	return (in.DownMbps + in.UpMbps) / 2
}

func healthFactor(s health.State) float64 {
	switch s {
	case health.Healthy:
		return 1
	case health.Degraded, health.Unknown:
		return 0.5
	}
	return 1
}

// Score implements S_i from the design. Stale metrics fall back to the
// configured-capacity weight of healthy lines instead of a random recompute.
func Score(in Input) float64 {
	pref := float64(in.Preference) / 100
	if pref <= 0 {
		pref = 1
	}
	base := capacity(in) * pref * healthFactor(in.State)
	if in.State == health.Recovering {
		ramp := 1.0
		if in.RecoverRamp > 0 {
			ramp = clamp(float64(in.RecoverElapsed)/float64(in.RecoverRamp), 0.05, 1)
		}
		base *= ramp
	}
	if !in.MetricsFresh {
		return base
	}
	// Use the same direction as capacity and saturation. Opposite-direction
	// traffic must not consume download/upload headroom a second time.
	u := Utilisation(in)
	headroom := math.Pow(math.Max(0.1, 1-clamp(u, 0, 1)), 0.7)
	// Headroom already steers new connections away from a busy line. Probe RTT
	// on that line is the same queue, so it must not cut the score a second time.
	lat := 1.0
	if !health.QueueingDelay(in.DownMbps, in.UpMbps, in.RXMbps, in.TXMbps) && in.BaseRTT > 0 && in.RTT > in.BaseRTT {
		excess := float64(in.RTT - in.BaseRTT)
		unit := 2 * math.Max(float64(in.BaseRTT), float64(10*time.Millisecond))
		lat = 1 / (1 + excess/unit)
	}
	loss := math.Pow(1-clamp(in.LossRate, 0, 0.5), 2)
	return base * headroom * lat * loss
}

func candidates(in []Input) []Input {
	var out []Input
	for _, w := range in {
		if w.Role != config.RoleBackup && w.State.Usable() {
			out = append(out, w)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, w := range in {
		if w.Role == config.RoleBackup && w.State.Usable() {
			out = append(out, w)
		}
	}
	return out
}

// Step computes the next shares.
func (s *Scheduler) Step(in []Input) Result {
	res := Result{Shares: map[int]int{}, Scores: map[int]float64{}, Reasons: map[int]string{}}
	cands := candidates(in)
	for _, w := range in {
		switch {
		case !w.State.Usable():
			res.Reasons[w.ID] = "down"
		case w.Role == config.RoleBackup:
			res.Reasons[w.ID] = "backup standby"
		}
	}
	if len(cands) == 0 {
		res.Changed = len(s.applied) != 0
		s.smoothed, s.applied, s.cands = map[int]float64{}, map[int]float64{}, map[int]bool{}
		s.saturated = map[int]bool{}
		return res
	}

	// Spill requires fresh load evidence for every possible destination. An
	// unmeasured line is not evidence of spare capacity, even in capacity mode.
	freshLoad := true
	for _, w := range cands {
		freshLoad = freshLoad && w.MetricsFresh
	}
	spill := map[int]bool{}
	if s.p.Saturation > 0 && freshLoad && (s.p.Objective == config.ObjectiveCapacity || comparableMetrics(cands)) {
		s.updateSaturation(cands)
		if len(s.saturated) > 0 && len(s.saturated) < len(cands) {
			spill = s.saturated
		}
	} else {
		// Unknown load is not a safe spill destination. Discard hysteresis
		// until evidence is complete again; RTT objectives also need RTTs.
		s.saturated = map[int]bool{}
	}
	res.Saturated = make(map[int]bool, len(spill))
	for id := range spill {
		res.Saturated[id] = true
	}

	raw := map[int]float64{}
	var sum float64
	var scores map[int]float64
	var reasons map[int]string
	if s.p.Objective != config.ObjectiveCapacity {
		scores, reasons = s.objectiveScores(cands)
	}
	for _, w := range cands {
		var sc float64
		if scores == nil {
			sc = Score(w)
		} else {
			sc = scores[w.ID]
		}
		if reasons[w.ID] != "" {
			res.Reasons[w.ID] = reasons[w.ID]
		}
		res.Scores[w.ID] = sc
		if spill[w.ID] {
			// Never erase relative health, preference or recovery weights. At
			// threshold the extra factor is 1; overload reduces it to at least
			// .5. Ordinary headroom already accounts for load. Avoid the hard
			// zero-score target that drives delayed-feedback ping-pong.
			sc *= clamp((1-Utilisation(w))/(1-s.p.Saturation), .5, 1)
			res.Reasons[w.ID] = "saturated: bounded score discount with gradual redistribution"
		}
		raw[w.ID] = sc
		sum += sc
	}
	if sum <= 0 {
		for id := range raw {
			raw[id] = 1
		}
		sum = float64(len(raw))
	}
	target := map[int]float64{}
	for id, sc := range raw {
		raw[id] = 100 * sc / sum
	}
	for id, v := range raw {
		if old, ok := s.smoothed[id]; ok {
			v = s.p.Alpha*v + (1-s.p.Alpha)*old
		}
		s.smoothed[id] = v // retain accumulated intent even below the applied deadband
		target[id] = v
	}
	for id := range s.smoothed {
		if _, ok := raw[id]; !ok {
			delete(s.smoothed, id)
		}
	}
	target = s.applyMinShare(target)

	newSet := map[int]bool{}
	for id := range target {
		newSet[id] = true
	}
	topologyChanged := len(newSet) != len(s.cands)
	for id := range newSet {
		if !s.cands[id] {
			topologyChanged = true
		}
	}

	var next map[int]float64
	switch {
	case len(s.applied) == 0 || topologyChanged && s.lostCandidate(newSet):
		// First run or a failed/removed line: take effect immediately.
		next = target
	default:
		next = s.rateLimit(target, newSet, nil)
		if !topologyChanged && maxDelta(next, s.applied) < s.p.Deadband {
			next = s.applied
		}
	}
	res.Shares = roundShares(next, s.p.MinShare)
	res.Changed = !sameShares(res.Shares, roundShares(s.applied, s.p.MinShare))
	s.applied = next
	s.cands = newSet
	return res
}

func (s *Scheduler) lostCandidate(newSet map[int]bool) bool {
	for id := range s.cands {
		if !newSet[id] {
			return true
		}
	}
	return false
}

// applyMinShare floors every candidate at MinShare and rescales the rest.
func (s *Scheduler) applyMinShare(t map[int]float64) map[int]float64 {
	out := map[int]float64{}
	floored := map[int]bool{}
	for iter := 0; iter < len(t)+1; iter++ {
		var freeSum, fixed float64
		for id, v := range t {
			if floored[id] {
				fixed += s.p.MinShare
			} else {
				freeSum += v
			}
		}
		changed := false
		for id, v := range t {
			if floored[id] {
				out[id] = s.p.MinShare
				continue
			}
			nv := v
			if freeSum > 0 {
				nv = v * (100 - fixed) / freeSum
			}
			if nv < s.p.MinShare {
				floored[id] = true
				changed = true
			}
			out[id] = nv
		}
		if !changed {
			break
		}
	}
	return out
}

// rateLimit moves applied toward target by at most MaxStep points per WAN and
// repairs the sum using headroom on the remaining WANs.
func (s *Scheduler) rateLimit(target map[int]float64, set, spill map[int]bool) map[int]float64 {
	out := map[int]float64{}
	for id, tv := range target {
		if spill[id] {
			out[id] = s.p.MinShare
			continue
		}
		cur, ok := s.applied[id]
		if !ok {
			cur = s.p.MinShare
		}
		out[id] = cur + clamp(tv-cur, -s.p.MaxStep, s.p.MaxStep)
	}
	appliedOrMin := func(id int) float64 {
		if cur, ok := s.applied[id]; ok {
			return cur
		}
		return s.p.MinShare
	}
	for iter := 0; iter < 8; iter++ {
		var sum float64
		for _, v := range out {
			sum += v
		}
		diff := 100 - sum
		if math.Abs(diff) < 1e-9 {
			break
		}
		// room each line has to move in the needed direction within its step limit
		room := map[int]float64{}
		var total float64
		for id := range set {
			// A spill floor is a hard constraint, not spare correction room.
			if spill[id] {
				continue
			}
			cur := appliedOrMin(id)
			var r float64
			if diff > 0 {
				r = cur + s.p.MaxStep - out[id]
			} else {
				r = out[id] - math.Max(cur-s.p.MaxStep, s.p.MinShare)
			}
			if r > 1e-9 {
				room[id] = r
				total += r
			}
		}
		if total == 0 {
			break
		}
		scale := math.Min(1, math.Abs(diff)/total)
		for id, r := range room {
			if diff > 0 {
				out[id] += r * scale
			} else {
				out[id] -= r * scale
			}
		}
	}
	// Final normalization must also preserve the fixed spill floors.
	var freeSum, fixed float64
	for id, v := range out {
		if spill[id] {
			fixed += v
		} else {
			freeSum += v
		}
	}
	if freeSum > 0 && math.Abs(freeSum+fixed-100) > 1e-6 {
		for id := range out {
			if !spill[id] {
				out[id] = out[id] * (100 - fixed) / freeSum
			}
		}
	}
	return out
}

func maxDelta(a, b map[int]float64) float64 {
	var m float64
	for id, v := range a {
		m = math.Max(m, math.Abs(v-b[id]))
	}
	for id, v := range b {
		if _, ok := a[id]; !ok {
			m = math.Max(m, v)
		}
	}
	return m
}

func sameShares(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// roundShares converts to integer percentages summing to exactly 100 with the
// largest-remainder method, keeping every candidate at least minShare points.
func roundShares(f map[int]float64, minShare float64) map[int]int {
	out := map[int]int{}
	if len(f) == 0 {
		return out
	}
	type rem struct {
		id   int
		frac float64
	}
	var rems []rem
	total := 0
	for id, v := range f {
		n := int(math.Floor(v))
		out[id] = n
		total += n
		rems = append(rems, rem{id, v - float64(n)})
	}
	sort.Slice(rems, func(i, j int) bool {
		if rems[i].frac != rems[j].frac {
			return rems[i].frac > rems[j].frac
		}
		return rems[i].id < rems[j].id
	})
	for i := 0; total < 100; i, total = (i+1)%len(rems), total+1 {
		out[rems[i].id]++
	}
	floor := int(math.Ceil(minShare))
	for id := range out {
		for out[id] < floor {
			// take one point from the largest share
			maxID := id
			for oid, v := range out {
				if v > out[maxID] {
					maxID = oid
				}
			}
			if maxID == id || out[maxID] <= floor {
				break
			}
			out[maxID]--
			out[id]++
		}
	}
	return out
}

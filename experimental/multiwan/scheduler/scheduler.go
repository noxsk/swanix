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
	MinShare float64 // percent for every usable candidate, default 1
	MaxStep  float64 // max percentage points per update, default 10
	Deadband float64 // changes below this many points are ignored, default 3
	Alpha    float64 // EWMA factor for target shares, default 0.2
	// Saturation (0 = off, else 0.5–0.99) is the utilisation of the configured
	// capacity at which a line counts as saturated. While at least one other
	// candidate is below it, saturated lines only keep MinShare and new
	// connections go to the unsaturated ones at once. A line is released when it
	// falls saturationRelease below the threshold.
	Saturation float64
}

// saturationRelease is the hysteresis below the threshold.
const saturationRelease = 0.10

func (p Params) withDefaults() Params {
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
	Saturated map[int]bool // lines that are spilling over to unsaturated ones
}

// Scheduler keeps the smoothing state between updates.
type Scheduler struct {
	p         Params
	smoothed  map[int]float64
	applied   map[int]float64
	cands     map[int]bool
	saturated map[int]bool // above the threshold (with hysteresis), whether or not spilling
	spilling  map[int]bool // saturated lines held at MinShare in the last step
}

func New(p Params) *Scheduler {
	return &Scheduler{p: p.withDefaults(), smoothed: map[int]float64{}, applied: map[int]float64{}, cands: map[int]bool{},
		saturated: map[int]bool{}, spilling: map[int]bool{}}
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
	u := 0.0
	if in.DownMbps > 0 {
		u = in.RXMbps / in.DownMbps
	}
	if in.UpMbps > 0 {
		u = math.Max(u, in.TXMbps/in.UpMbps)
	}
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
		s.smoothed, s.applied, s.cands = map[int]float64{}, map[int]float64{}, map[int]bool{}
		res.Changed = true
		return res
	}

	// spill-over: saturated lines give their new connections to unsaturated ones
	spill := map[int]bool{}
	if s.p.Saturation > 0 {
		s.updateSaturation(cands)
		if len(s.saturated) > 0 && len(s.saturated) < len(cands) {
			spill = s.saturated
		}
	}
	res.Saturated = spill

	raw := map[int]float64{}
	var sum float64
	for _, w := range cands {
		sc := Score(w)
		res.Scores[w.ID] = sc
		if spill[w.ID] {
			sc = 0
			res.Reasons[w.ID] = "saturated: new connections go to unsaturated lines"
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
		if old, ok := s.smoothed[id]; ok && !spill[id] {
			v = s.p.Alpha*v + (1-s.p.Alpha)*old
		}
		s.smoothed[id] = v // a spilling line restarts from zero and ramps back afterwards
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

	newSpill := false
	for id := range spill {
		if !s.spilling[id] {
			newSpill = true
		}
	}
	s.spilling = spill

	var next map[int]float64
	switch {
	case len(s.applied) == 0 || topologyChanged && s.lostCandidate(newSet) || newSpill:
		// first run, a line left the pool, or a line just saturated: take effect immediately
		next = target
	default:
		next = s.rateLimit(target, newSet)
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
func (s *Scheduler) rateLimit(target map[int]float64, set map[int]bool) map[int]float64 {
	out := map[int]float64{}
	for id, tv := range target {
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
	// final exact normalisation (only matters if every line hit its step limit)
	var sum float64
	for _, v := range out {
		sum += v
	}
	if sum > 0 && math.Abs(sum-100) > 1e-6 {
		for id := range out {
			out[id] = out[id] * 100 / sum
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

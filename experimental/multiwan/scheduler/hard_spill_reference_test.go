package scheduler

import (
	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// Frozen pre-soft-spill Step control flow, retained only as the A/B reference.
// Basic scoring/rounding helpers are shared; this compares saturation control,
// not the unrelated historical score or Quality implementations.
type hardSpillReference struct {
	*Scheduler
	smooth   bool         // experiment: retain hard zero-score target but obey EWMA/step
	spilling map[int]bool // historical hard-floor transition state
}

func newHardSpillReference(p Params) *hardSpillReference {
	return &hardSpillReference{Scheduler: New(p)}
}

func (s *hardSpillReference) Step(in []Input) Result {
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
		s.saturated, s.spilling = map[int]bool{}, map[int]bool{}
		return res
	}
	fresh := true
	for _, w := range cands {
		fresh = fresh && w.MetricsFresh
	}
	spill := map[int]bool{}
	if s.p.Saturation > 0 && fresh && (s.p.Objective == config.ObjectiveCapacity || comparableMetrics(cands)) {
		s.updateSaturation(cands)
		if len(s.saturated) > 0 && len(s.saturated) < len(cands) {
			spill = s.saturated
		}
	} else {
		s.saturated = map[int]bool{}
	}
	res.Saturated = spill
	raw := map[int]float64{}
	var sum float64
	var scores map[int]float64
	var reasons map[int]string
	if s.p.Objective != config.ObjectiveCapacity {
		scores, reasons = s.objectiveScores(cands)
	}
	for _, w := range cands {
		sc := Score(w)
		if scores != nil {
			sc = scores[w.ID]
		}
		if reasons[w.ID] != "" {
			res.Reasons[w.ID] = reasons[w.ID]
		}
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
		if old, ok := s.smoothed[id]; ok && (!spill[id] || s.smooth) {
			v = s.p.Alpha*v + (1-s.p.Alpha)*old
		}
		s.smoothed[id] = v
		target[id] = v
	}
	for id := range s.smoothed {
		if _, ok := raw[id]; !ok {
			delete(s.smoothed, id)
		}
	}
	target = s.applyMinShare(target)
	set := map[int]bool{}
	for id := range target {
		set[id] = true
	}
	topology := len(set) != len(s.cands)
	for id := range set {
		if !s.cands[id] {
			topology = true
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
	case len(s.applied) == 0 || topology && s.lostCandidate(set) || newSpill && !s.smooth:
		next = target
	default:
		fixed := spill
		if s.smooth {
			fixed = nil
		}
		next = s.rateLimit(target, set, fixed)
		if !topology && maxDelta(next, s.applied) < s.p.Deadband {
			next = s.applied
		}
	}
	res.Shares = roundShares(next, s.p.MinShare)
	res.Changed = !sameShares(res.Shares, roundShares(s.applied, s.p.MinShare))
	s.applied, s.cands = next, set
	return res
}

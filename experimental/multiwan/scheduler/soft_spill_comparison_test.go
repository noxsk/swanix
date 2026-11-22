package scheduler

import (
	"fmt"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"math"
	"slices"
	"testing"
)

func TestSoftSpillComparison(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveBalanced} {
		for _, interval := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s/%ds", objective, interval), func(t *testing.T) {
				p := Params{Objective: objective, Saturation: .9}
				hard := runFeedbackController(t, newHardSpillReference(p), interval, 100, false, true)
				smooth := newHardSpillReference(p)
				smooth.smooth = true
				limited := runFeedbackController(t, smooth, interval, 100, false, false)
				got := runFeedbackController(t, New(p), interval, 100, false, false)
				reversed := runFeedbackController(t, New(p), interval, 100, true, false)
				fine := runFeedbackController(t, New(p), interval, 1000, false, false)
				p.Saturation = 0
				off := runFeedbackModel(t, p, interval, 100, false)
				for i, st := range got {
					q := func(v feedbackStats) float64 { return v.delays[len(v.delays)*95/100] }
					t.Logf("%s hard/step-only/soft/off Mbps=%.2f/%.2f/%.2f/%.2f; queue p95=%.2f/%.2f/%.2f/%.2f; late transitions=%d/%d/%d", feedbackPhases[i].name, hard[i].served/60, limited[i].served/60, st.served/60, off[i].served/60, q(hard[i]), q(limited[i]), q(st), q(off[i]), hard[i].lateSpillChanges, limited[i].lateSpillChanges, st.lateSpillChanges)
					if !slices.Equal(st.trace, reversed[i].trace) || math.Abs(st.served-fine[i].served)/60 > 1 {
						t.Fatal("ordering/integration regression")
					}
					if i == 1 || i == 2 {
						if st.lateSpillChanges > hard[i].lateSpillChanges {
							t.Fatal("persistent saturation churn regression")
						}
						if st.served < hard[i].served || st.served < .98*off[i].served || q(st) > q(off[i])*1.1+1 {
							t.Fatal("service/queue guardrail failed")
						}
					}
				}
				if got[0].dropped > 1e-6 || got[3].endQueue > 1e-6 || got[3].dropped > 1e-6 {
					t.Fatal("light-load/recovery regression")
				}
			})
		}
	}
}

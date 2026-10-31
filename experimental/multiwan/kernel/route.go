package kernel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// unreachableMetric keeps the terminating route behind any real default route.
const unreachableMetric = 4278198272

// Priorities are offsets from kernel.priority_base.
const (
	probeRuleOffset    = 10  // + WAN ID
	suppressRuleOffset = 100 // lookup main, ignoring its default routes
	businessOffset     = 200 // + WAN ID
	rejectOffset       = 250
	rangeWidth         = 256
)

// BusinessTable and ProbeTable return the routing table numbers of a WAN.
func BusinessTable(base, id int) int { return base + id }
func ProbeTable(base, id int) int    { return base + 256 + id }

// Rule is one desired ip rule.
type Rule struct {
	Family   int
	Priority int
	Match    string // e.g. "fwmark 0x1000000/0xf000000"; empty for suppress rule
	Action   string // "lookup 1001", "lookup main suppress_prefixlength 0", "unreachable"
}

// Route is one desired route inside a managed table.
type Route struct {
	Family int
	Table  int
	Spec   string // everything after "ip route replace", without "table N"
}

// Key makes rules comparable.
func (r Rule) Key() string {
	return fmt.Sprintf("%d|%d|%s|%s", r.Family, r.Priority, r.Match, r.Action)
}

func (r Route) Key() string { return fmt.Sprintf("%d|%d|%s", r.Family, r.Table, r.Spec) }

// Plan is the desired routing state.
type Plan struct {
	Rules  []Rule
	Routes []Route
}

// BuildRoutePlan derives the desired ip rules and per-WAN tables.
func (in *Input) BuildRoutePlan() (*Plan, error) {
	lay, err := NewLayout(in.Cfg.Kernel.MarkMask)
	if err != nil {
		return nil, err
	}
	k := in.Cfg.Kernel
	p := &Plan{}
	idm := hex(lay.IDMask())
	for _, fam := range []int{4, 6} {
		if !in.familyEnabled(fam) {
			continue
		}
		for _, id := range in.enabledIDs() {
			rt, ok := in.wan(id)
			bt, pt := BusinessTable(k.TableBase, id), ProbeTable(k.TableBase, id)
			p.Rules = append(p.Rules,
				Rule{fam, k.PriorityBase + probeRuleOffset + id,
					fmt.Sprintf("fwmark %s/%s", hex(lay.ProbeMark(id)), hex(lay.IDMask()|lay.Probe())), fmt.Sprintf("lookup %d", pt)},
				Rule{fam, k.PriorityBase + businessOffset + id,
					fmt.Sprintf("fwmark %s/%s", hex(lay.WANMark(id)), idm), fmt.Sprintf("lookup %d", bt)},
			)
			for _, tbl := range []int{bt, pt} {
				p.Routes = append(p.Routes, Route{fam, tbl, fmt.Sprintf("unreachable default metric %d", unreachableMetric)})
				if ok {
					if spec := defaultRoute(rt, fam); spec != "" {
						p.Routes = append(p.Routes, Route{fam, tbl, spec})
					}
				}
			}
		}
		p.Rules = append(p.Rules,
			Rule{fam, k.PriorityBase + suppressRuleOffset, "", "lookup main suppress_prefixlength 0"},
			Rule{fam, k.PriorityBase + rejectOffset, fmt.Sprintf("fwmark %s/%s", hex(lay.WANMark(RejectID)), idm), "unreachable"},
		)
	}
	sort.SliceStable(p.Rules, func(i, j int) bool { return p.Rules[i].Priority < p.Rules[j].Priority })
	return p, nil
}

// defaultRoute builds the default route of a WAN. Point-to-point links (PPPoE)
// have no gateway; link-local IPv6 gateways always carry their device scope.
func defaultRoute(w WANRuntime, fam int) string {
	if fam == 4 {
		if !w.Up4 || w.Device4 == "" {
			return ""
		}
		spec := "default"
		if w.Gateway4.IsValid() {
			spec += " via " + w.Gateway4.String()
		}
		spec += " dev " + w.Device4
		if w.Src4.IsValid() {
			spec += " src " + w.Src4.String()
		}
		return spec + " metric 1"
	}
	if !w.Up6 || w.Device6 == "" {
		return ""
	}
	spec := "default"
	if w.Gateway6.IsValid() {
		spec += " via " + w.Gateway6.String()
	}
	return spec + " dev " + w.Device6 + " metric 1"
}

// OwnsPriority reports whether an ip rule priority belongs to swanix.
func (in *Input) OwnsPriority(prio int) bool {
	b := in.Cfg.Kernel.PriorityBase
	return prio >= b && prio < b+rangeWidth
}

func famFlag(fam int) string {
	if fam == 6 {
		return "-6"
	}
	return "-4"
}

func itoa(n int) string { return strconv.Itoa(n) }

func fields(s string) []string { return strings.Fields(s) }

package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Runner executes external commands. Arguments are always passed as an array,
// never through a shell.
type Runner interface {
	Run(stdin string, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(stdin string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Applier reconciles the kernel with a desired Input.
type Applier struct {
	R Runner
	// NoIPv6 skips IPv6 routing state on kernels built without IPv6.
	NoIPv6 bool
	// ProcSys and StateDir default to /proc/sys and /var/run/swanix; tests override them.
	ProcSys  string
	StateDir string
}

func NewApplier() *Applier {
	_, err := os.Stat("/proc/net/if_inet6")
	return &Applier{R: ExecRunner{}, NoIPv6: err != nil}
}

func (a *Applier) families() []int {
	if a.NoIPv6 {
		return []int{4}
	}
	return []int{4, 6}
}

// Apply installs routes and rules first, then the nft selectors. The two
// subsystems have no joint transaction, so selectors only go live once the
// routes they point at exist.
func (a *Applier) Apply(in *Input) (*Ruleset, error) {
	plan, err := in.BuildRoutePlan()
	if err != nil {
		return nil, err
	}
	rs, err := in.Render()
	if err != nil {
		return nil, err
	}
	if err := a.ensureLooseRPFilter(in); err != nil {
		return nil, fmt.Errorf("rp_filter: %w", err)
	}
	if err := a.syncRoutes(in, plan, true); err != nil {
		return nil, fmt.Errorf("routes: %w", err)
	}
	if err := a.syncRules(in, plan); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if _, err := a.R.Run(rs.Script, "nft", "-f", "-"); err != nil {
		return nil, fmt.Errorf("nftables: %w", err)
	}
	return rs, nil
}

// UpdateRuntime applies interface/address changes incrementally: routes and
// rules are reconciled, then sets, selection chains and weights are refreshed
// in one atomic nft batch. The nft table itself is not recreated.
func (a *Applier) UpdateRuntime(in *Input) error {
	plan, err := in.BuildRoutePlan()
	if err != nil {
		return err
	}
	rs, err := in.RenderRuntime()
	if err != nil {
		return err
	}
	if err := a.ensureLooseRPFilter(in); err != nil {
		return fmt.Errorf("rp_filter: %w", err)
	}
	if err := a.syncRoutes(in, plan, false); err != nil {
		return fmt.Errorf("routes: %w", err)
	}
	if err := a.syncRules(in, plan); err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	if _, err := a.R.Run(rs.Script, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

// Counters returns how many new connections each WAN has been assigned
// (key RejectID for rejected ones), from the per-WAN nft counters.
func (a *Applier) Counters(in *Input) (map[int]uint64, error) {
	out, err := a.R.Run("", "nft", "-j", "list", "counters", "table", "inet", in.Cfg.Kernel.NFTTable)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse nft counters: %w", err)
	}
	res := map[int]uint64{}
	for _, e := range doc.Nftables {
		if e.Counter == nil {
			continue
		}
		switch {
		case e.Counter.Name == "sel_reject":
			res[RejectID] = e.Counter.Packets
		case strings.HasPrefix(e.Counter.Name, "sel_wan"):
			var id int
			if _, err := fmt.Sscanf(e.Counter.Name, "sel_wan%d", &id); err == nil {
				res[id] = e.Counter.Packets
			}
		}
	}
	return res, nil
}

// UpdateWeights refreshes only the weight maps (single atomic nft batch).
func (a *Applier) UpdateWeights(in *Input) error {
	_, err := a.R.Run(in.RenderWeights(), "nft", "-f", "-")
	return err
}

// Remove withdraws everything swanix owns, in reverse order of Apply.
func (a *Applier) Remove(in *Input) error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if out, err := a.R.Run("", "nft", "list", "tables"); err == nil && strings.Contains(out, "inet "+in.Cfg.Kernel.NFTTable+"\n") {
		_, err := a.R.Run("", "nft", "delete", "table", "inet", in.Cfg.Kernel.NFTTable)
		keep(err)
	}
	empty := &Plan{}
	keep(a.syncRules(in, empty))
	keep(a.syncRoutes(in, empty, true))
	keep(a.restoreRPFilter())
	return firstErr
}

type ipRuleJSON struct {
	Priority    int    `json:"priority"`
	Src         string `json:"src"`
	Fwmark      string `json:"fwmark"`
	Fwmask      string `json:"fwmask"`
	Table       string `json:"table"`
	Action      string `json:"action"`
	SuppressLen *int   `json:"suppress_prefixlen"`
}

func (a *Applier) syncRules(in *Input, plan *Plan) error {
	want := map[string]Rule{}
	for _, r := range plan.Rules {
		if a.NoIPv6 && r.Family == 6 {
			continue
		}
		want[r.Key()] = r
	}
	for _, fam := range a.families() {
		out, err := a.R.Run("", "ip", famFlag(fam), "-j", "rule", "show")
		if err != nil {
			return err
		}
		var have []ipRuleJSON
		if err := json.Unmarshal([]byte(out), &have); err != nil {
			return fmt.Errorf("parse ip rule: %w", err)
		}
		present := map[string]bool{}
		for _, h := range have {
			if !in.OwnsPriority(h.Priority) {
				continue
			}
			key := a.ruleKeyFromJSON(fam, h)
			if _, ok := want[key]; ok {
				present[key] = true
				continue
			}
			if err := a.delRule(fam, h.Priority); err != nil {
				return err
			}
		}
		var missing []Rule
		for k, r := range want {
			if r.Family == fam && !present[k] {
				missing = append(missing, r)
			}
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i].Priority < missing[j].Priority })
		for _, r := range missing {
			if err := a.addRule(r); err != nil {
				return err
			}
		}
	}
	return nil
}

// ruleKeyFromJSON reconstructs the canonical key of an installed rule.
func (a *Applier) ruleKeyFromJSON(fam int, h ipRuleJSON) string {
	r := Rule{Family: fam, Priority: h.Priority}
	if h.Fwmark != "" {
		mask := h.Fwmask
		if mask == "" {
			mask = "0xffffffff"
		}
		r.Match = fmt.Sprintf("fwmark %s/%s", normHex(h.Fwmark), normHex(mask))
	}
	switch {
	case h.Action == "unreachable":
		r.Action = "unreachable"
	case h.SuppressLen != nil:
		r.Action = fmt.Sprintf("lookup %s suppress_prefixlength %d", h.Table, *h.SuppressLen)
	default:
		r.Action = "lookup " + h.Table
	}
	return r.Key()
}

func normHex(s string) string {
	var v uint32
	if _, err := fmt.Sscanf(strings.ToLower(s), "0x%x", &v); err != nil {
		return s
	}
	return hex(v)
}

func (a *Applier) addRule(r Rule) error {
	args := []string{famFlag(r.Family), "rule", "add"}
	args = append(args, fields(r.Match)...)
	args = append(args, "priority", itoa(r.Priority))
	args = append(args, fields(r.Action)...)
	_, err := a.R.Run("", "ip", args...)
	return err
}

func (a *Applier) delRule(fam, prio int) error {
	_, err := a.R.Run("", "ip", famFlag(fam), "rule", "del", "priority", itoa(prio))
	return err
}

type ipRouteJSON struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
	Type    string `json:"type"`
	Metric  int64  `json:"metric"`
	Prefsrc string `json:"prefsrc"`
}

// syncRoutes reconciles the routing tables. sweep also visits every table swanix could own
// (stale tables of removed WANs); incremental updates only touch the planned ones.
func (a *Applier) syncRoutes(in *Input, plan *Plan, sweep bool) error {
	type tf struct{ fam, tbl int }
	wantBy := map[tf]map[string]Route{}
	for _, r := range plan.Routes {
		if a.NoIPv6 && r.Family == 6 {
			continue
		}
		k := tf{r.Family, r.Table}
		if wantBy[k] == nil {
			wantBy[k] = map[string]Route{}
		}
		wantBy[k][r.Spec] = r
	}
	// every table we may have touched: planned ones plus all owned IDs
	tables := map[tf]bool{}
	for k := range wantBy {
		tables[k] = true
	}
	for id := 1; sweep && id <= MaxID; id++ {
		for _, fam := range a.families() {
			tables[tf{fam, BusinessTable(in.Cfg.Kernel.TableBase, id)}] = true
			tables[tf{fam, ProbeTable(in.Cfg.Kernel.TableBase, id)}] = true
		}
	}
	keys := make([]tf, 0, len(tables))
	for k := range tables {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tbl != keys[j].tbl {
			return keys[i].tbl < keys[j].tbl
		}
		return keys[i].fam < keys[j].fam
	})
	for _, k := range keys {
		want := wantBy[k]
		for _, spec := range sortedSpecs(want) {
			if _, err := a.R.Run("", "ip", append([]string{famFlag(k.fam), "route", "replace"}, append(fields(spec), "table", itoa(k.tbl))...)...); err != nil {
				return err
			}
		}
		// list again: replace may have rewritten routes we saw before
		have, err := a.listRoutes(k.fam, k.tbl)
		if err != nil {
			return err
		}
		for _, h := range have {
			if routeWanted(h, want) {
				continue
			}
			args := []string{famFlag(k.fam), "route", "del"}
			if h.Type == "unreachable" {
				args = append(args, "unreachable")
			}
			dst := h.Dst
			if dst == "" {
				dst = "default"
			}
			args = append(args, dst)
			if h.Gateway != "" {
				args = append(args, "via", h.Gateway)
			}
			if h.Dev != "" {
				args = append(args, "dev", h.Dev)
			}
			args = append(args, "table", itoa(k.tbl))
			if _, err := a.R.Run("", "ip", args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Applier) listRoutes(fam, tbl int) ([]ipRouteJSON, error) {
	out, err := a.R.Run("", "ip", famFlag(fam), "-j", "route", "show", "table", itoa(tbl))
	if err != nil {
		if strings.Contains(err.Error(), "FIB table does not exist") {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var have []ipRouteJSON
	if err := json.Unmarshal([]byte(out), &have); err != nil {
		return nil, fmt.Errorf("parse ip route: %w", err)
	}
	return have, nil
}

// MaxID is the highest regular WAN ID.
const MaxID = 14

func sortedSpecs(m map[string]Route) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// routeWanted compares an installed route against the desired specs by its
// semantic parts (type, gateway, device), not by textual form.
func routeWanted(h ipRouteJSON, want map[string]Route) bool {
	for spec := range want {
		f := fields(spec)
		isUnreach := f[0] == "unreachable"
		if isUnreach != (h.Type == "unreachable") {
			continue
		}
		if isUnreach {
			return true
		}
		var gw, dev string
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				gw = f[i+1]
			case "dev":
				dev = f[i+1]
			}
		}
		if h.Gateway == gw && h.Dev == dev {
			return true
		}
	}
	return false
}

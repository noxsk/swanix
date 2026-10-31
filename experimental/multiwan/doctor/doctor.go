// Package doctor checks that the system can safely be taken over by swanix.
// It only reads; nothing here changes the system.
package doctor

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
)

type Level string

const (
	OK   Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
)

// Check is one finding.
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Env is what the checks may look at.
type Env struct {
	Cfg      *config.Config
	Runner   kernel.Runner
	ReadFile func(string) ([]byte, error)
	Netif    netif.Source
	// SwanixRunning means our own objects may legitimately be present.
	SwanixRunning bool
}

func (e *Env) read(p string) string {
	rf := e.ReadFile
	if rf == nil {
		rf = os.ReadFile
	}
	b, err := rf(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *Env) run(name string, args ...string) (string, error) {
	return e.Runner.Run("", name, args...)
}

// Run executes every check. A result containing any Fail must block `apply`.
func Run(e *Env) []Check {
	var out []Check
	add := func(c ...Check) { out = append(out, c...) }
	add(checkTools(e)...)
	add(checkIPv6Kernel(e))
	snap, err := netif.Collect(e.Cfg, e.Netif)
	if err != nil {
		add(Check{Name: "netifd", Level: Fail, Detail: err.Error(), Fix: "确认 ubus 与 netifd 正在运行"})
	} else {
		add(checkInterfaces(e, snap)...)
		add(checkTopology(e, snap)...)
		add(checkRPFilter(e, snap)...)
	}
	add(checkConflicts(e)...)
	add(checkFirewall(e)...)
	return out
}

func hasLevel(cs []Check, l Level) bool {
	for _, c := range cs {
		if c.Level == l {
			return true
		}
	}
	return false
}

// HasFailure reports whether any check failed.
func HasFailure(cs []Check) bool { return hasLevel(cs, Fail) }

func checkTools(e *Env) []Check {
	var out []Check
	for _, t := range []struct {
		name string
		args []string
		fix  string
	}{
		{"nft", []string{"list", "tables"}, "安装 nftables 与 kmod-nft-core（firewall4 依赖它们）"},
		{"ip", []string{"-V"}, "安装 ip-full（需要 fwmark 与 suppress_prefixlength 规则支持）"},
		{"ubus", []string{"list", "network.interface"}, "确认 ubusd 与 netifd 正在运行"},
	} {
		if _, err := e.run(t.name, t.args...); err != nil {
			out = append(out, Check{Name: "tool:" + t.name, Level: Fail, Detail: err.Error(), Fix: t.fix})
		} else {
			out = append(out, Check{Name: "tool:" + t.name, Level: OK, Detail: "可用"})
		}
	}
	return out
}

func checkIPv6Kernel(e *Env) Check {
	if e.Cfg.IPv6.Mode == config.IPv6Unmanaged {
		return Check{Name: "ipv6", Level: OK, Detail: "ipv6.mode=unmanaged：不管理 IPv6"}
	}
	if e.read("/proc/net/if_inet6") == "" {
		return Check{Name: "ipv6", Level: Warn, Detail: "内核未启用 IPv6，IPv6 规则将被跳过", Fix: "如需 IPv6 请使用带 IPv6 的固件"}
	}
	return Check{Name: "ipv6", Level: OK, Detail: "内核支持 IPv6"}
}

func checkInterfaces(e *Env, snap *netif.Snapshot) []Check {
	var out []Check
	for _, p := range snap.Problems {
		out = append(out, Check{Name: "interface", Level: Fail, Detail: p, Fix: "修正 /etc/config/swanix 或 /etc/config/network"})
	}
	for _, w := range snap.WANs {
		if w.Device4 == "" {
			continue
		}
		lvl, detail := OK, fmt.Sprintf("%s → %s", w.Name, w.Device4)
		if !w.Up4 {
			lvl, detail = Warn, fmt.Sprintf("%s (%s) 当前未连通，将以 down 状态加入", w.Name, w.Device4)
		}
		out = append(out, Check{Name: "wan:" + w.Name, Level: lvl, Detail: detail})
		if len(w.Device4) > 15 {
			out = append(out, Check{Name: "wan:" + w.Name, Level: Fail, Detail: "设备名超过 15 字符，nftables ifname 无法容纳"})
		}
	}
	if len(snap.LAN.Devices) == 0 && e.Cfg.Global.Enabled {
		out = append(out, Check{Name: "lan", Level: Fail, Detail: "没有解析到任何受管 LAN 设备", Fix: "检查 global.lan_network"})
	}
	return out
}

// checkTopology reports overlapping upstream topologies that are not handled.
func checkTopology(e *Env, snap *netif.Snapshot) []Check {
	var out []Check
	gw, src := map[string]string{}, map[string]string{}
	for _, w := range snap.WANs {
		if w.Gateway4.IsValid() {
			if prev, dup := gw[w.Gateway4.String()]; dup {
				out = append(out, Check{Name: "topology", Level: Warn, Detail: fmt.Sprintf("%s 与 %s 使用相同的网关地址 %s，只按接口区分", w.Name, prev, w.Gateway4)})
			}
			gw[w.Gateway4.String()] = w.Name
		}
		if w.Src4.IsValid() {
			if prev, dup := src[w.Src4.String()]; dup {
				out = append(out, Check{Name: "topology", Level: Fail, Detail: fmt.Sprintf("%s 与 %s 的本机地址 %s 重复", w.Name, prev, w.Src4), Fix: "重复本机地址的重叠拓扑暂不支持，请调整上游网段"})
			}
			src[w.Src4.String()] = w.Name
		}
	}
	return out
}

func checkRPFilter(e *Env, snap *netif.Snapshot) []Check {
	var out []Check
	for _, w := range snap.WANs {
		if w.Device4 == "" {
			continue
		}
		v := strings.TrimSpace(e.read("/proc/sys/net/ipv4/conf/" + w.Device4 + "/rp_filter"))
		if v == "1" {
			out = append(out, Check{Name: "rp_filter:" + w.Device4, Level: Warn,
				Detail: "严格反向路径过滤会丢弃从其他线路返回的合法流量",
				Fix:    "swanix 应用时会仅将该 WAN 设备调为 loose(2)，不会全局关闭"})
		}
	}
	return out
}

var (
	markRe = regexp.MustCompile(`meta mark set ([^;\n]*)`)
	hexRe  = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	prioRe = regexp.MustCompile(`^(\d+):`)
	mwanRe = regexp.MustCompile(`mwan3`)
)

func checkConflicts(e *Env) []Check {
	var out []Check
	if rs, err := e.run("nft", "list", "ruleset"); err == nil {
		if mwanRe.MatchString(rs) {
			out = append(out, Check{Name: "mwan3", Level: Warn, Detail: "检测到 mwan3 的 nftables 规则",
				Fix: "apply 会先停止 mwan3 再启用 swanix；observe 模式可与 mwan3 共存"})
		}
		mask, _, err := config.ParseMarkMask(e.Cfg.Kernel.MarkMask)
		if err == nil {
			own := "table inet " + e.Cfg.Kernel.NFTTable + " {"
			rest := rs
			if i := strings.Index(rs, own); i >= 0 {
				j := strings.Index(rs[i:], "\n}\n")
				if j > 0 {
					rest = rs[:i] + rs[i+j:]
				}
			}
			if h, ok := overlappingMark(rest, mask); ok {
				out = append(out, Check{Name: "mark-mask", Level: Warn,
					Detail: fmt.Sprintf("其他规则写入的 mark 值 %s 与 kernel.mark_mask=%s 有重叠", h, e.Cfg.Kernel.MarkMask),
					Fix:    "换一个 mark_mask，或调整其他组件的 mark 位"})
			}
		}
	}
	if !e.SwanixRunning {
		for _, fam := range []string{"-4", "-6"} {
			rules, err := e.run("ip", fam, "rule", "show")
			if err != nil {
				continue
			}
			base := e.Cfg.Kernel.PriorityBase
			for _, line := range strings.Split(rules, "\n") {
				m := prioRe.FindStringSubmatch(strings.TrimSpace(line))
				if m == nil {
					continue
				}
				p, _ := strconv.Atoi(m[1])
				if p >= base && p < base+256 {
					out = append(out, Check{Name: "ip-rule", Level: Fail, Detail: fmt.Sprintf("优先级 %d 已被占用: %s", p, strings.TrimSpace(line)),
						Fix: "调整 kernel.priority_base 避开该区间"})
				}
			}
		}
	}
	return out
}

func zoneList(r config.RawSection, key string) []string {
	var out []string
	for _, v := range r.Options[key] {
		out = append(out, strings.Fields(v)...)
	}
	return out
}

func checkFirewall(e *Env) []Check {
	var out []Check
	txt := e.read("/etc/config/firewall")
	if txt == "" {
		return []Check{{Name: "fw4", Level: Warn, Detail: "无法读取 /etc/config/firewall", Fix: "确认 firewall4 已安装；swanix 依赖 fw4 的 NAT 与访问控制"}}
	}
	secs, err := config.ParseRaw(txt)
	if err != nil {
		return []Check{{Name: "fw4", Level: Warn, Detail: "无法解析 /etc/config/firewall: " + err.Error()}}
	}
	for _, s := range secs {
		if s.Type == "defaults" {
			out = append(out, checkOffload(e.Cfg.Global.FlowOffload, s.Options)...)
		}
	}
	if !e.Cfg.Global.ManageRouterTraffic {
		return out
	}
	for _, w := range e.Cfg.WANs {
		if !w.Enabled {
			continue
		}
		covered := false
		for _, s := range secs {
			if s.Type != "zone" {
				continue
			}
			inZone := false
			for _, n := range zoneList(s, "network") {
				if n == w.Network {
					inZone = true
				}
			}
			if !inZone {
				continue
			}
			covered = true
			masq := len(s.Options["masq"]) > 0 && s.Options["masq"][0] == "1"
			switch {
			case !masq:
				out = append(out, Check{Name: "fw4-masq:" + w.Name, Level: Fail,
					Detail: "WAN 所在防火墙区域未启用 masq，重路由后的本机 IPv4 流量不会被 SNAT",
					Fix:    "在该 zone 上设置 masq '1'"})
			case len(s.Options["masq_src"]) > 0 || len(s.Options["masq_dest"]) > 0:
				out = append(out, Check{Name: "fw4-masq:" + w.Name, Level: Fail,
					Detail: "zone 的 masq_src/masq_dest 限制可能使本机流量不被 SNAT",
					Fix:    "移除 masq_src/masq_dest 限制，或将 manage_router_traffic 设为 0"})
			default:
				out = append(out, Check{Name: "fw4-masq:" + w.Name, Level: OK, Detail: "zone 已覆盖该 WAN 的 SNAT"})
			}
			if e.Cfg.IPv6.Mode == config.IPv6NAT66 && len(s.Options["masq6"]) > 0 && s.Options["masq6"][0] == "1" {
				out = append(out, Check{Name: "nat66-double", Level: Fail,
					Detail: "zone 已启用 masq6，与 swanix 的 NAT66 重复", Fix: "关闭该 zone 的 masq6，由 swanix 统一做 NAT66"})
			}
		}
		if !covered {
			out = append(out, Check{Name: "fw4-zone:" + w.Name, Level: Fail, Detail: "没有防火墙区域包含网络 " + w.Network, Fix: "把该 WAN 放入 wan 区域"})
		}
	}
	return out
}

// overlappingMark finds a constant that other rules write into a mark and that
// touches our mask. AND-masks (preserving bits) are not writes and are ignored.
func overlappingMark(ruleset string, mask uint32) (string, bool) {
	for _, m := range markRe.FindAllStringSubmatch(ruleset, -1) {
		toks := strings.Fields(m[1])
		prev := "set"
		for _, t := range toks {
			if hexRe.MatchString(t) && (prev == "|" || prev == "set" || prev == "or" || prev == "xor") {
				if v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(t), "0x"), 16, 32); err == nil && uint32(v)&mask != 0 {
					return t, true
				}
			}
			prev = t
		}
	}
	return "", false
}

// checkOffload compares fw4's flow offloading with global.flow_offload. Software
// offload is compatible (offloaded flows keep the route of their first packets,
// which swanix chose) and has to be declared; hardware offload is not verified.
func checkOffload(declared string, opts map[string][]string) []Check {
	on := func(k string) bool { v := opts[k]; return len(v) > 0 && v[0] == "1" }
	sw, hw := on("flow_offloading"), on("flow_offloading_hw")
	switch {
	case hw:
		return []Check{{Name: "flow_offload", Level: Fail,
			Detail: "fw4 已启用硬件 flow offloading（flow_offloading_hw），该组合尚未在硬件上验收",
			Fix:    "关闭 flow_offloading_hw；软件 offload 可保留（同时设置 global.flow_offload software）"}}
	case sw && declared != config.OffloadSoftware:
		return []Check{{Name: "flow_offload", Level: Fail,
			Detail: "fw4 已启用软件 flow offloading，但 global.flow_offload 为 " + declared,
			Fix:    "设置 global.flow_offload software，或在防火墙设置中关闭 flow offloading"}}
	case !sw && declared == config.OffloadSoftware:
		return []Check{{Name: "flow_offload", Level: Warn,
			Detail: "global.flow_offload 为 software，但 fw4 未启用 flow offloading",
			Fix:    "两处保持一致"}}
	case sw:
		return []Check{{Name: "flow_offload", Level: OK, Detail: "软件 flow offloading：已建立连接沿用首包选定的线路"}}
	}
	return nil
}

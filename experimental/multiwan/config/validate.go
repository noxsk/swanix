package config

import (
	"fmt"
	"math/bits"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	MinWANID = 1
	MaxWANID = 14 // 15 is reserved as the reject/terminate marker
)

var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// BuildProfile describes what the running binary can honour.
type BuildProfile struct {
	Name         string // "lite" or "full"
	ProxyAllowed bool
}

var (
	ProfileLite = BuildProfile{Name: "lite"}
	ProfileFull = BuildProfile{Name: "full", ProxyAllowed: true}
)

// ParseMarkMask parses kernel.mark_mask and returns the mask and its shift.
// The mask must be eight contiguous bits.
func ParseMarkMask(s string) (mask uint32, shift uint, err error) {
	n, perr := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 32)
	if perr != nil || n == 0 {
		return 0, 0, fmt.Errorf("必须是十六进制掩码，例如 0xff000000")
	}
	mask = uint32(n)
	shift = uint(bits.TrailingZeros32(mask))
	if bits.OnesCount32(mask) != 8 || mask>>shift != 0xff {
		return 0, 0, fmt.Errorf("必须是连续 8 位")
	}
	return mask, shift, nil
}

// EffectiveDefaultPolicy returns the policy that catches unmatched traffic.
func (c *Config) EffectiveDefaultPolicy() string {
	if c.Global.DefaultPolicy != "" {
		return c.Global.DefaultPolicy
	}
	if len(c.Policies) == 1 {
		return c.Policies[0].Name
	}
	return ""
}

// Validate performs strict semantic validation.
func (c *Config) Validate(profile BuildProfile) error {
	var e Errors
	g := c.Global

	if g.ConfigVersion != CurrentVersion {
		e.addf("global.config_version", "运行配置迁移或设置为 1", "不支持的配置版本 %d", g.ConfigVersion)
	}
	if g.StartupMode != StartupObserve && g.StartupMode != StartupManage {
		e.addf("global.startup_mode", "使用 observe 或 manage", "非法取值 %q", g.StartupMode)
	}
	switch g.FlowOffload {
	case OffloadOff, OffloadSoftware:
	case OffloadHardware:
		e.addf("global.flow_offload", "使用 software 或 off", "hardware offload 尚未在硬件上验收（software 已在 VM 中验证）")
	default:
		e.addf("global.flow_offload", "使用 off", "非法取值 %q", g.FlowOffload)
	}
	if g.ConfirmTimeout < 10 || g.ConfirmTimeout > 600 {
		e.add("global.confirm_timeout", "必须在 10–600 秒之间", "")
	}
	if g.UpdateInterval < 2 || g.UpdateInterval > 300 {
		e.add("global.update_interval", "必须在 2–300 秒之间", "")
	}
	if g.SampleInterval < 1 || g.SampleInterval > g.UpdateInterval {
		e.add("global.sample_interval", "必须在 1 秒与 update_interval 之间", "")
	}
	if g.Enabled && len(g.LANNetworks) == 0 {
		e.add("global.lan_network", "至少声明一个受管 LAN 逻辑网络", "")
	}
	seenLAN := map[string]bool{}
	for _, n := range g.LANNetworks {
		if !identRe.MatchString(n) {
			e.addf("global.lan_network", "使用 netifd 逻辑接口名", "非法接口名 %q", n)
		}
		if seenLAN[n] {
			e.addf("global.lan_network", "删除重复项", "重复的 LAN %q", n)
		}
		seenLAN[n] = true
	}

	k := c.Kernel
	if _, _, err := ParseMarkMask(k.MarkMask); err != nil {
		e.add("kernel.mark_mask", err.Error(), "")
	}
	if k.TableBase < 1 || k.TableBase > 60000 {
		e.add("kernel.table_base", "必须在 1–60000 之间", "")
	}
	if k.PriorityBase < 1 || k.PriorityBase > 30000 {
		e.add("kernel.priority_base", "必须在 1–30000 之间", "")
	}
	if !identRe.MatchString(k.NFTTable) {
		e.add("kernel.nft_table", "只能包含字母数字和下划线", "")
	}
	if !strings.HasPrefix(k.ControlSocket, "/var/run/") {
		e.add("kernel.control_socket", "必须位于 /var/run 下", "")
	}

	// WANs
	ids := map[int]string{}
	nets := map[string]string{}
	activeCount := 0
	if g.Enabled && len(c.WANs) == 0 {
		e.add("wan", "至少配置一条 WAN", "")
	}
	for _, w := range c.WANs {
		p := "wan." + w.Name
		if !identRe.MatchString(w.Name) {
			e.add(p, "section 名只能包含字母数字和下划线", "")
		}
		if w.ID < MinWANID || w.ID > MaxWANID {
			e.addf(p+".id", "使用 1–14 的稳定编号", "WAN ID %d 越界", w.ID)
		} else if prev, dup := ids[w.ID]; dup {
			e.addf(p+".id", "为每条 WAN 分配唯一 ID", "与 wan.%s 重复", prev)
		} else {
			ids[w.ID] = w.Name
		}
		if w.Network == "" || !identRe.MatchString(w.Network) {
			e.addf(p+".network", "填写 netifd 逻辑接口名", "接口 %q 无效", w.Network)
		} else if prev, dup := nets[w.Network]; dup {
			e.addf(p+".network", "一个逻辑接口只能属于一条 WAN", "与 wan.%s 重复接口归属", prev)
		} else {
			nets[w.Network] = w.Name
		}
		if w.IPv6Network != "auto" && w.IPv6Network != "none" && !identRe.MatchString(w.IPv6Network) {
			e.addf(p+".ipv6_network", "使用 auto、none 或逻辑接口名", "非法取值 %q", w.IPv6Network)
		}
		if !(w.DownloadMbps > 0) {
			e.add(p+".download_mbps", "必须大于 0，单位 Mbps", "")
		}
		if !(w.UploadMbps > 0) {
			e.add(p+".upload_mbps", "必须大于 0，单位 Mbps", "")
		}
		if w.Preference < 1 || w.Preference > 1000 {
			e.add(p+".preference", "必须在 1–1000 之间", "")
		}
		switch w.Role {
		case RoleActive:
			if w.Enabled {
				activeCount++
			}
		case RoleBackup:
		default:
			e.addf(p+".role", "使用 active 或 backup", "非法取值 %q", w.Role)
		}
		switch w.TrafficBias {
		case BiasDownload, BiasBalanced, BiasUpload:
		default:
			e.addf(p+".traffic_bias", "使用 download/balanced/upload", "非法取值 %q", w.TrafficBias)
		}
		if w.HealthRef != "" {
			if _, ok := c.healthByName(w.HealthRef); !ok {
				e.addf(p+".health", "引用已存在的 health section", "未找到 health.%s", w.HealthRef)
			}
		}
		for _, t := range w.ExtraTargets {
			if err := validateTarget(t); err != nil {
				e.add(p+".probe_target", err.Error(), "")
			}
		}
	}
	if g.Enabled && len(c.WANs) > 0 && activeCount == 0 {
		e.add("wan", "至少需要一条启用的 active WAN", "")
	}

	// Health
	for _, h := range c.Health {
		p := "health." + h.Name
		if len(h.Targets) == 0 {
			e.add(p+".target", "至少配置一个探测目标", "")
		}
		for _, t := range h.Targets {
			if err := validateTarget(t); err != nil {
				e.add(p+".target", err.Error(), "")
			}
		}
		if h.Interval < 1 {
			e.add(p+".interval", "必须 ≥ 1 秒", "")
		}
		if h.Timeout < 1 || h.Timeout > h.Interval*2 {
			e.add(p+".timeout", "必须在 1 秒与 2×interval 之间", "")
		}
		if h.SuccessesRequired < 1 || h.SuccessesRequired > len(h.Targets) {
			e.add(p+".successes_required", "必须在 1 与目标数量之间", "")
		}
		if h.DownRounds < 1 {
			e.add(p+".down_rounds", "必须 ≥ 1", "")
		}
		if h.UpRounds < 1 {
			e.add(p+".up_rounds", "必须 ≥ 1", "")
		}
		if h.RecoverRamp < 0 || h.RecoverRamp > 3600 {
			e.add(p+".recover_ramp", "必须在 0–3600 秒之间", "")
		}
	}

	// Policies
	for _, pol := range c.Policies {
		p := "policy." + pol.Name
		if !identRe.MatchString(pol.Name) {
			e.add(p, "section 名只能包含字母数字和下划线", "")
		}
		if len(pol.Members) == 0 {
			e.add(p+".member", "候选列表不能为空", "")
		}
		seen := map[string]bool{}
		for _, m := range pol.Members {
			if _, ok := c.WANByName(m); !ok {
				e.addf(p+".member", "引用已存在的 wan section", "未找到 wan.%s", m)
			}
			if seen[m] {
				e.addf(p+".member", "删除重复成员", "引用的 WAN %q 重复", m)
			}
			seen[m] = true
		}
		switch pol.Affinity {
		case AffinityConnection, AffinitySourceDestination, AffinitySource:
		default:
			e.addf(p+".affinity", "使用 connection/source_destination/source", "非法取值 %q", pol.Affinity)
		}
		if pol.AffinityTimeout < 10 || pol.AffinityTimeout > 86400 {
			e.add(p+".affinity_timeout", "必须在 10–86400 秒之间", "")
		}
		if t := pol.SaturationThreshold; t != 0 && (t < 50 || t > 99) {
			e.add(p+".saturation_threshold", "0（关闭）或 50–99（占配置带宽的百分比）", "")
		}
		if pol.UnavailableAction != UnavailableReject {
			e.addf(p+".unavailable_action", "目前仅支持 reject，不会静默回退 main", "不支持 %q", pol.UnavailableAction)
		}
	}
	if g.DefaultPolicy != "" {
		if _, ok := c.PolicyByName(g.DefaultPolicy); !ok {
			e.addf("global.default_policy", "引用已存在的 policy section", "未找到 policy.%s", g.DefaultPolicy)
		}
	} else if g.Enabled && c.EffectiveDefaultPolicy() == "" {
		e.add("global.default_policy", "存在多个 policy 时必须显式指定默认策略", "")
	}

	// Rules
	for _, r := range c.Rules {
		p := "rule." + r.Name
		if _, ok := c.PolicyByName(r.Policy); !ok {
			e.addf(p+".policy", "引用已存在的 policy section", "未找到 policy.%s", r.Policy)
		}
		if r.Priority < 1 || r.Priority > 9999 {
			e.add(p+".priority", "必须在 1–9999 之间", "")
		}
		switch r.Proto {
		case "", "tcp", "udp", "icmp", "icmpv6":
		default:
			e.addf(p+".proto", "使用 tcp/udp/icmp/icmpv6 或留空", "非法取值 %q", r.Proto)
		}
		for _, s := range r.Src {
			if _, err := parsePrefixOrAddr(s); err != nil {
				e.addf(p+".src", "使用 IP 或 CIDR", "无效地址 %q", s)
			}
		}
		for _, s := range r.Dest {
			if _, err := parsePrefixOrAddr(s); err != nil {
				e.addf(p+".dest", "使用 IP 或 CIDR", "无效地址 %q", s)
			}
		}
		if len(r.DestPort) > 0 && r.Proto != "tcp" && r.Proto != "udp" {
			e.add(p+".dest_port", "指定端口时 proto 必须为 tcp 或 udp", "")
		}
		for _, port := range r.DestPort {
			if err := validatePortSpec(port); err != nil {
				e.addf(p+".dest_port", "使用 80 或 8000-8100", "无效端口 %q", port)
			}
		}
	}

	// IPv6
	switch c.IPv6.Mode {
	case IPv6Native, IPv6NAT66, IPv6Unmanaged:
	default:
		e.addf("ipv6.mode", "使用 native/nat66/unmanaged", "非法取值 %q", c.IPv6.Mode)
	}
	if c.IPv6.Mode == IPv6NAT66 {
		if len(c.IPv6.NAT66Sources) == 0 {
			e.add("ipv6.nat66_source", "nat66 需要显式授权的 LAN 源前缀", "")
		}
		for _, s := range c.IPv6.NAT66Sources {
			pf, err := netip.ParsePrefix(s)
			if err != nil || !pf.Addr().Is6() {
				e.addf("ipv6.nat66_source", "使用 IPv6 CIDR", "无效前缀 %q", s)
			}
		}
	}

	// Proxy
	switch c.Proxy.TransparentMode {
	case TransparentOff, TransparentTUN:
		// tun: the TUN inbound in proxy.json is checked against swanix's rules, tables and marks when it is loaded
	case TransparentTProxy:
		e.addf("proxy.transparent_mode", "使用 tun 或 off",
			"tproxy 需要自行维护的 nft/ip rule，swanix 无法核对其 mark 与规则，尚不支持")
	default:
		e.addf("proxy.transparent_mode", "使用 off", "非法取值 %q", c.Proxy.TransparentMode)
	}
	if c.Proxy.Enabled && !profile.ProxyAllowed {
		e.addf("proxy.enabled", "改用 Full 构建，或将其设为 0", "当前 %s 构建不含代理模块", profile.Name)
	}
	if !c.Proxy.Enabled && c.Proxy.TransparentMode != TransparentOff {
		e.add("proxy.transparent_mode", "仅在 proxy.enabled=1 时可使用", "")
	}

	return e.OrNil()
}

func (c *Config) healthByName(n string) (Health, bool) {
	for _, h := range c.Health {
		if h.Name == n {
			return h, true
		}
	}
	return Health{}, false
}

func parsePrefixOrAddr(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func validatePortSpec(s string) error {
	lo, hi, found := strings.Cut(s, "-")
	a, err := strconv.Atoi(lo)
	if err != nil || a < 1 || a > 65535 {
		return fmt.Errorf("bad port")
	}
	if !found {
		return nil
	}
	b, err := strconv.Atoi(hi)
	if err != nil || b < a || b > 65535 {
		return fmt.Errorf("bad port range")
	}
	return nil
}

// validateTarget accepts host:port (TCP connect) or an http(s):// URL.
func validateTarget(t string) error {
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		u, err := url.Parse(t)
		if err != nil || u.Host == "" {
			return fmt.Errorf("无效探测 URL %q", t)
		}
		if _, err := netip.ParseAddr(u.Hostname()); err != nil {
			return fmt.Errorf("探测目标 %q 必须使用 IP 地址，以保证名称解析不经由其他 WAN", t)
		}
		return nil
	}
	host, port, err := splitHostPort(t)
	if err != nil || host == "" {
		return fmt.Errorf("探测目标 %q 需要 host:port 或 http(s):// URL", t)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("探测目标 %q 必须使用 IP 地址，以保证名称解析不经由其他 WAN", t)
	}
	if err := validatePortSpec(port); err != nil || strings.Contains(port, "-") {
		return fmt.Errorf("探测目标 %q 端口无效", t)
	}
	return nil
}

func splitHostPort(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 || end+2 > len(s) || s[end+1] != ':' {
			return "", "", fmt.Errorf("bad host:port")
		}
		return s[1:end], s[end+2:], nil
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("bad host:port")
	}
	return s[:i], s[i+1:], nil
}

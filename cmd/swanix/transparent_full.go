//go:build !mw_lite

package main

import (
	"fmt"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	E "github.com/sagernet/sing/common/exceptions"
)

// tunRuleSpan is how far past iproute2_rule_index sing-tun's auto_route rules reach
// (its "nop" target sits at index+10).
const tunRuleSpan = 10

// checkSharedResources holds proxy.json to the kernel resources swanix owns: the
// mark window, the ip rule priority range and the routing table numbers. With
// transparent_mode tun the TUN's auto_route rules must come before swanix's, so
// LAN traffic enters the TUN and the proxy's wan_policy sockets (source-bound and
// marked) are routed by swanix's tables.
func checkSharedResources(cfg *config.Config, options option.Options) error {
	mask, _, err := config.ParseMarkMask(cfg.Kernel.MarkMask)
	if err != nil {
		return err
	}
	k := cfg.Kernel
	inMarks := func(field string, v uint32) error {
		if v&mask != 0 {
			return E.New(field, fmt.Sprintf(": 0x%x 与 kernel.mark_mask %s 重叠，请改用该掩码之外的位", v, k.MarkMask))
		}
		return nil
	}
	if options.Route != nil {
		if err := inMarks("route.default_mark", uint32(options.Route.DefaultMark)); err != nil {
			return err
		}
	}
	usesPolicy := false
	dialers := func(kind, tag string, opts any) error {
		w, ok := opts.(option.DialerOptionsWrapper)
		if !ok {
			return nil
		}
		d := w.TakeDialerOptions()
		if d.WANPolicy != "" {
			usesPolicy = true
		}
		return inMarks(kind+"["+tag+"].routing_mark", uint32(d.RoutingMark))
	}
	for _, o := range options.Outbounds {
		if err := dialers("outbounds", o.Tag, o.Options); err != nil {
			return err
		}
	}
	for _, e := range options.Endpoints {
		if err := dialers("endpoints", e.Tag, e.Options); err != nil {
			return err
		}
	}

	var tuns []string
	for _, in := range options.Inbounds {
		t, ok := in.Options.(*option.TunInboundOptions)
		if in.Type != C.TypeTun || !ok || !t.AutoRoute {
			continue
		}
		tuns = append(tuns, in.Tag)
		field := "inbounds[" + in.Tag + "]"
		if cfg.Proxy.TransparentMode != config.TransparentTUN {
			return E.New(field, ": auto_route 的 TUN 需要 proxy.transparent_mode tun，swanix 才会核对其规则与 mark")
		}
		rule := t.IPRoute2RuleIndex
		if rule == 0 {
			rule = tun.DefaultIPRoute2RuleIndex
		}
		if rule+tunRuleSpan >= k.PriorityBase {
			return E.New(field, fmt.Sprintf(".iproute2_rule_index: %d–%d 必须位于 kernel.priority_base %d 之前，LAN 流量才会先进入 TUN", rule, rule+tunRuleSpan, k.PriorityBase))
		}
		table := t.IPRoute2TableIndex
		if table == 0 {
			table = tun.DefaultIPRoute2TableIndex
		}
		if table >= k.TableBase && table < k.TableBase+512 {
			return E.New(field, fmt.Sprintf(".iproute2_table_index: %d 落在 swanix 的路由表范围 %d–%d 内", table, k.TableBase, k.TableBase+511))
		}
		if t.AutoRedirect {
			// sing-tun's redirect rules exempt only packets whose mark equals its output
			// mark; probes and wan_policy sockets carry a line mark and would be pulled back
			// into the proxy (verified in namespaces: lines never became healthy)
			return E.New(field, ".auto_redirect: 不能与 swanix 同用：其 nftables 重定向只放过 mark 恰为 auto_redirect_output_mark 的连接，探测与 wan_policy 连接会被重新拉回代理；请只用 auto_route")
		}
	}
	if cfg.Proxy.TransparentMode == config.TransparentTUN {
		if len(tuns) == 0 {
			return E.New("proxy.transparent_mode tun: ", cfg.Proxy.ConfigFile, " 中没有启用 auto_route 的 tun 入站")
		}
		if !usesPolicy {
			return E.New("proxy.transparent_mode tun: 没有出站设置 wan_policy，TUN 接管的流量只会经由一条线路离开")
		}
	}
	return nil
}

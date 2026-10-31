// Package netif reads interface state from netifd (via ubus) and watches the
// kernel for address, route and link changes.
package netif

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
)

// Interface is the subset of a netifd interface status we use.
type Interface struct {
	Name      string
	Up        bool
	Available bool
	Device    string
	Addrs4    []netip.Prefix
	Addrs6    []netip.Prefix
	Prefixes6 []netip.Prefix // delegated prefixes and the /64s assigned from them
	Gateway4  netip.Addr
	Gateway6  netip.Addr
}

type addrJSON struct {
	Address string `json:"address"`
	Mask    int    `json:"mask"`
}

type prefixJSON struct {
	addrJSON
	Assigned map[string]addrJSON `json:"assigned"`
}

type routeJSON struct {
	Target  string `json:"target"`
	Mask    int    `json:"mask"`
	Nexthop string `json:"nexthop"`
}

type statusJSON struct {
	Up         bool         `json:"up"`
	Available  bool         `json:"available"`
	L3Device   string       `json:"l3_device"`
	Device     string       `json:"device"`
	IPv4       []addrJSON   `json:"ipv4-address"`
	IPv6       []addrJSON   `json:"ipv6-address"`
	IPv6Prefix []prefixJSON `json:"ipv6-prefix"`
	PrefixAsg  []addrJSON   `json:"ipv6-prefix-assignment"`
	Routes     []routeJSON  `json:"route"`
}

func prefixOf(a addrJSON) (netip.Prefix, bool) {
	ip, err := netip.ParseAddr(a.Address)
	if err != nil || a.Mask < 0 || a.Mask > ip.BitLen() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, a.Mask), true
}

// ParseStatus decodes the output of `ubus call network.interface.X status`.
func ParseStatus(name string, data []byte) (Interface, error) {
	var s statusJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return Interface{}, fmt.Errorf("netifd status of %s: %w", name, err)
	}
	ifc := Interface{Name: name, Up: s.Up, Available: s.Available, Device: s.L3Device}
	if ifc.Device == "" {
		ifc.Device = s.Device
	}
	for _, a := range s.IPv4 {
		if p, ok := prefixOf(a); ok {
			ifc.Addrs4 = append(ifc.Addrs4, p)
		}
	}
	for _, a := range s.IPv6 {
		if p, ok := prefixOf(a); ok {
			ifc.Addrs6 = append(ifc.Addrs6, p)
		}
	}
	for _, pf := range s.IPv6Prefix {
		for _, asg := range pf.Assigned {
			if p, ok := prefixOf(asg); ok {
				ifc.Prefixes6 = append(ifc.Prefixes6, p.Masked())
			}
		}
	}
	for _, a := range s.PrefixAsg {
		if p, ok := prefixOf(a); ok {
			ifc.Addrs6 = append(ifc.Addrs6, p)
		}
	}
	for _, r := range s.Routes {
		if r.Mask != 0 || r.Nexthop == "" {
			continue
		}
		gw, err := netip.ParseAddr(r.Nexthop)
		if err != nil || gw.IsUnspecified() {
			continue
		}
		switch r.Target {
		case "0.0.0.0":
			ifc.Gateway4 = gw
		case "::":
			ifc.Gateway6 = gw
		}
	}
	return ifc, nil
}

// Source fetches interface status. The production implementation calls ubus.
type Source interface {
	Status(name string) (Interface, bool, error)
}

type UbusSource struct{ R kernel.Runner }

func NewUbusSource() *UbusSource { return &UbusSource{R: kernel.ExecRunner{}} }

func (u *UbusSource) Status(name string) (Interface, bool, error) {
	out, err := u.R.Run("", "ubus", "call", "network.interface."+name, "status")
	if err != nil {
		if strings.Contains(err.Error(), "Not found") {
			return Interface{}, false, nil
		}
		return Interface{}, false, err
	}
	ifc, err := ParseStatus(name, []byte(out))
	return ifc, true, err
}

// Snapshot is the resolved runtime view used by the engine.
type Snapshot struct {
	WANs     []kernel.WANRuntime
	LAN      kernel.LANRuntime
	Problems []string // unresolved interfaces and other soft failures
}

// Collect resolves every configured WAN and LAN against netifd.
func Collect(cfg *config.Config, src Source) (*Snapshot, error) {
	snap := &Snapshot{}
	cache := map[string]*Interface{}
	get := func(name string) (*Interface, error) {
		if c, ok := cache[name]; ok {
			return c, nil
		}
		ifc, found, err := src.Status(name)
		if err != nil {
			return nil, err
		}
		if !found {
			cache[name] = nil
			return nil, nil
		}
		cache[name] = &ifc
		return &ifc, nil
	}

	for _, w := range cfg.WANs {
		if !w.Enabled {
			continue
		}
		rt := kernel.WANRuntime{ID: w.ID, Name: w.Name}
		v4, err := get(w.Network)
		if err != nil {
			return nil, err
		}
		if v4 == nil {
			snap.Problems = append(snap.Problems, fmt.Sprintf("wan.%s.network: netifd 接口 %s 不存在", w.Name, w.Network))
		} else {
			rt.Device4 = v4.Device
			rt.Up4 = v4.Up && v4.Available && len(v4.Addrs4) > 0
			rt.Gateway4 = v4.Gateway4
			if len(v4.Addrs4) > 0 {
				rt.Src4 = v4.Addrs4[0].Addr()
			}
		}
		v6, perr := resolveV6(cfg, w, v4, get)
		if perr != "" {
			snap.Problems = append(snap.Problems, perr)
		}
		if v6 != nil {
			rt.Device6 = v6.Device
			rt.Up6 = v6.Up && v6.Available && v6.Gateway6.IsValid()
			rt.Gateway6 = v6.Gateway6
			rt.Prefixes6 = v6.Prefixes6
		}
		snap.WANs = append(snap.WANs, rt)
	}

	for _, n := range cfg.Global.LANNetworks {
		ifc, err := get(n)
		if err != nil {
			return nil, err
		}
		if ifc == nil {
			snap.Problems = append(snap.Problems, fmt.Sprintf("global.lan_network: netifd 接口 %s 不存在", n))
			continue
		}
		if ifc.Device != "" {
			snap.LAN.Devices = append(snap.LAN.Devices, ifc.Device)
		}
		for _, p := range ifc.Addrs4 {
			snap.LAN.Prefixes4 = append(snap.LAN.Prefixes4, p.Masked())
		}
		for _, p := range ifc.Addrs6 {
			snap.LAN.Prefixes6 = append(snap.LAN.Prefixes6, p.Masked())
		}
	}
	return snap, nil
}

// resolveV6 implements ipv6_network: "none", an explicit interface, or "auto".
// Auto only resolves when exactly one candidate exists: the WAN's own interface
// when it carries an IPv6 default route, "<network>6" or netifd's "<network>_6".
func resolveV6(cfg *config.Config, w config.WAN, v4 *Interface, get func(string) (*Interface, error)) (*Interface, string) {
	switch w.IPv6Network {
	case "none":
		return nil, ""
	case "auto":
		var cands []*Interface
		if v4 != nil && v4.Gateway6.IsValid() {
			cands = append(cands, v4)
		}
		// "<network>6" is the conventional companion section; "<network>_6" is the
		// dynamic interface netifd creates for PPPoE/DHCP with `option ipv6 auto`
		for _, name := range []string{w.Network + "6", w.Network + "_6"} {
			if c, err := get(name); err == nil && c != nil {
				cands = append(cands, c)
			}
		}
		if len(cands) == 1 {
			return cands[0], ""
		}
		if len(cands) > 1 {
			return nil, fmt.Sprintf("wan.%s.ipv6_network: auto 无法唯一解析，请显式指定", w.Name)
		}
		return nil, ""
	}
	c, err := get(w.IPv6Network)
	if err != nil || c == nil {
		return nil, fmt.Sprintf("wan.%s.ipv6_network: netifd 接口 %s 不存在", w.Name, w.IPv6Network)
	}
	return c, ""
}

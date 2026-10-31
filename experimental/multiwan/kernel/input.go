package kernel

import (
	"net/netip"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// WANRuntime is what the interface layer learned from netifd for one WAN.
type WANRuntime struct {
	ID        int
	Name      string
	Device4   string // L3 device, e.g. eth1 or pppoe-wan
	Gateway4  netip.Addr
	Src4      netip.Addr
	Up4       bool
	Device6   string
	Gateway6  netip.Addr // may be link-local; always paired with Device6
	Prefixes6 []netip.Prefix
	Up6       bool
	// Down6 marks a WAN that is link-up but failed its health probes; native
	// IPv6 then rejects its source prefixes instead of sending them elsewhere.
	Down6 bool
}

// LANRuntime lists the managed LAN devices and their on-link prefixes.
type LANRuntime struct {
	Devices   []string
	Prefixes4 []netip.Prefix
	Prefixes6 []netip.Prefix
}

// Shares maps policy name -> address family (4 or 6) -> WAN ID -> percent.
type Shares map[string]map[int]map[int]int

// Input is everything needed to render the data path.
type Input struct {
	Cfg    *config.Config
	WANs   []WANRuntime
	LAN    LANRuntime
	Shares Shares
}

func (in *Input) wan(id int) (WANRuntime, bool) {
	for _, w := range in.WANs {
		if w.ID == id {
			return w, true
		}
	}
	return WANRuntime{}, false
}

// enabledIDs returns the IDs of enabled WANs in ascending order.
func (in *Input) enabledIDs() []int {
	var ids []int
	for _, w := range in.Cfg.WANs {
		if w.Enabled {
			ids = append(ids, w.ID)
		}
	}
	sortInts(ids)
	return ids
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

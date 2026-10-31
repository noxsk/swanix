package engine

import (
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
	"github.com/sagernet/sing-box/experimental/multiwan/scheduler"
)

// Preview builds the kernel input the daemon would install right now, with
// every up WAN treated as healthy and weighted by configured capacity. It
// writes nothing.
func Preview(cfg *config.Config, src netif.Source) (*kernel.Input, *netif.Snapshot, error) {
	snap, err := netif.Collect(cfg, src)
	if err != nil {
		return nil, nil, err
	}
	shares := kernel.Shares{}
	for _, pol := range cfg.Policies {
		shares[pol.Name] = map[int]map[int]int{}
		for _, fam := range []int{4, 6} {
			if fam == 6 && cfg.IPv6.Mode != config.IPv6NAT66 {
				continue
			}
			var in []scheduler.Input
			for _, name := range pol.Members {
				w, ok := cfg.WANByName(name)
				if !ok || !w.Enabled {
					continue
				}
				state := health.Down
				for _, r := range snap.WANs {
					if r.ID == w.ID && ((fam == 4 && r.Up4) || (fam == 6 && r.Up6)) {
						state = health.Healthy
					}
				}
				in = append(in, scheduler.Input{ID: w.ID, State: state, Role: w.Role, DownMbps: w.DownloadMbps,
					UpMbps: w.UploadMbps, Preference: w.Preference, Bias: w.TrafficBias})
			}
			shares[pol.Name][fam] = scheduler.New(scheduler.Params{}).Step(in).Shares
		}
	}
	return &kernel.Input{Cfg: cfg, WANs: snap.WANs, LAN: snap.LAN, Shares: shares}, snap, nil
}

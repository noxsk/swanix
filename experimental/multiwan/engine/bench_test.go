package engine

import (
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
)

// BenchmarkPickParallel: the line choice the Full build makes for every proxied
// connection (wan_policy), from many goroutines at once.
func BenchmarkPickParallel(b *testing.B) {
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		b.Fatal(err)
	}
	e, err := New(Options{Config: cfg, Profile: config.ProfileLite, Version: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	e.shares = kernel.Shares{"balanced": {4: {1: 60, 2: 40}}}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := e.Pick("balanced", 4); err != nil {
				b.Fatal(err)
			}
		}
	})
}

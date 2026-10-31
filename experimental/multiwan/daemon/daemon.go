// Package daemon runs the MultiWAN engine together with its control socket.
// Lite calls it directly; Full's sing-box service calls it from Start.
package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/buildinfo"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/control"
	"github.com/sagernet/sing-box/experimental/multiwan/engine"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
)

// Params configures a daemon.
type Params struct {
	Config  *config.Config
	Profile config.BuildProfile
	// ProxyFeatures are the compiled-in proxy capabilities (empty for Lite).
	ProxyFeatures []string
	// OnEngine is called once the engine exists, before it starts running.
	OnEngine func(*engine.Engine)
}

// NewEngine builds a production engine.
func NewEngine(p Params) (*engine.Engine, error) {
	return engine.New(engine.Options{
		Config:  p.Config,
		Profile: p.Profile,
		Version: buildinfo.Version,
		Netif:   netif.NewUbusSource(),
		Kernel:  kernel.NewApplier(),
		Dial:    health.MarkedDial,
		Logf:    log.Printf,
	})
}

// Run blocks until ctx is done.
func Run(ctx context.Context, p Params) error {
	eng, err := NewEngine(p)
	if err != nil {
		return err
	}
	if p.OnEngine != nil {
		p.OnEngine(eng)
	}
	errc := make(chan error, 2)
	go func() {
		err := netif.Watch(ctx, 500*time.Millisecond, eng.Poke)
		if err != nil && ctx.Err() == nil {
			// events are an optimisation: the 30 s reconcile still covers us
			log.Printf("netlink watch stopped: %v (falling back to periodic reconcile)", err)
		}
	}()
	go func() {
		errc <- control.Serve(ctx, p.Config.Kernel.ControlSocket, func(method string) (any, error) {
			switch method {
			case control.MethodStatus:
				return eng.Status(), nil
			case control.MethodHistory:
				return eng.History(), nil
			case control.MethodFeature:
				return buildinfo.Collect(p.ProxyFeatures), nil
			case control.MethodPoke:
				eng.Poke()
				return map[string]bool{"queued": true}, nil
			}
			return nil, fmt.Errorf("unknown method %q", method)
		})
	}()
	go func() { errc <- eng.Run(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

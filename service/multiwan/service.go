// Package multiwan adapts the swanix MultiWAN engine to the sing-box service
// registry (Full build only). The engine itself lives in experimental/multiwan
// and knows nothing about sing-box.
package multiwan

import (
	"context"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/daemon"
	"github.com/sagernet/sing-box/experimental/multiwan/engine"
	"github.com/sagernet/sing-box/experimental/multiwan/wanhook"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
)

const DefaultConfigFile = "/etc/config/swanix"

func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.MultiWANServiceOptions](registry, C.TypeMultiWAN, NewService)
}

type Service struct {
	boxService.Adapter
	ctx    context.Context
	logger log.ContextLogger
	cfg    *config.Config

	// ProxyFeatures reported through `features`.
	proxyFeatures []string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ProxyFeaturesFunc supplies the compiled-in proxy capability list; set by the
// Full entry before the service is created.
var ProxyFeaturesFunc = func() []string { return nil }

func NewService(ctx context.Context, logger log.ContextLogger, tag string, options option.MultiWANServiceOptions) (adapter.Service, error) {
	path := options.ConfigFile
	if path == "" {
		path = DefaultConfigFile
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return nil, E.Cause(err, "load ", path)
	}
	if err := cfg.Validate(config.ProfileFull); err != nil {
		return nil, E.Cause(err, "validate ", path)
	}
	return &Service{
		Adapter:       boxService.NewAdapter(C.TypeMultiWAN, tag),
		ctx:           ctx,
		logger:        logger,
		cfg:           cfg,
		proxyFeatures: ProxyFeaturesFunc(),
	}, nil
}

func (s *Service) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if !s.cfg.Global.Enabled {
		s.logger.Info("global.enabled=0, MultiWAN service idle")
		return nil
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := daemon.Run(ctx, daemon.Params{
			Config:        s.cfg,
			Profile:       config.ProfileFull,
			ProxyFeatures: s.proxyFeatures,
			// dialers pick their WAN through this selector right before connect
			OnEngine: func(e *engine.Engine) { wanhook.Register(e) },
		})
		if err != nil {
			s.logger.Error("multiwan: ", err)
		}
	}()
	scope.Add(func() error {
		wanhook.Register(nil) // dials now fail closed instead of picking an arbitrary line
		s.cancel()
		s.wg.Wait()
		return nil
	})
	return nil
}

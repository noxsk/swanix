//go:build !mw_lite

package main

import (
	"context"
	"os"
	"sort"

	"github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/deprecated"
	"github.com/sagernet/sing-box/experimental/multiwan/buildinfo"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/daemon"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/service/multiwan"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

// The Full entry keeps the regular sing-box runtime. With proxy.enabled=0 it
// behaves like Lite; with proxy.enabled=1 it starts sing-box from
// /etc/swanix/proxy.json and registers MultiWAN as one of its services.

var profile = config.ProfileFull

const multiwanTag = "swanix-multiwan"

func init() {
	buildinfo.Profile = "full"
	multiwan.ProxyFeaturesFunc = proxyFeatures
}

// proxyFeatures lists what this binary can really do, read from the live
// registries rather than from a hand-written list.
func proxyFeatures() []string {
	var out []string
	add := func(kind string, types []string) {
		for _, t := range types {
			out = append(out, kind+":"+t)
		}
	}
	add("inbound", include.InboundRegistry().OptionTypes())
	add("outbound", include.OutboundRegistry().OptionTypes())
	add("endpoint", include.EndpointRegistry().OptionTypes())
	add("dns", include.DNSTransportRegistry().OptionTypes())
	add("service", include.ServiceRegistry().OptionTypes())
	add("certificate", include.CertificateProviderRegistry().OptionTypes())
	sort.Strings(out)
	return out
}

func runDaemon(ctx context.Context, cfg *config.Config) error {
	if !cfg.Proxy.Enabled {
		return daemon.Run(ctx, daemon.Params{Config: cfg, Profile: profile, ProxyFeatures: proxyFeatures()})
	}
	return runProxy(ctx, cfg)
}

func newProxyContext(parent context.Context) context.Context {
	return include.Context(service.ContextWith(parent, deprecated.NewStderrManager(log.StdLogger())))
}

// loadProxyOptions reads proxy.json, injects the multiwan service and checks
// every wan_policy reference. The user's file is never modified.
func loadProxyOptions(ctx context.Context, cfg *config.Config) (option.Options, error) {
	content, err := os.ReadFile(cfg.Proxy.ConfigFile)
	if err != nil {
		return option.Options{}, E.Cause(err, "read ", cfg.Proxy.ConfigFile)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, content)
	if err != nil {
		return option.Options{}, E.Cause(err, "decode ", cfg.Proxy.ConfigFile)
	}
	for _, s := range options.Services {
		if s.Type == C.TypeMultiWAN {
			return option.Options{}, E.New("services: the multiwan service is injected by swanix, remove the entry with tag ", s.Tag, " from ", cfg.Proxy.ConfigFile)
		}
		if s.Tag == multiwanTag {
			return option.Options{}, E.New("services: tag ", multiwanTag, " is reserved by swanix")
		}
	}
	if err := checkWANPolicies(cfg, options); err != nil {
		return option.Options{}, err
	}
	if err := checkSharedResources(cfg, options); err != nil {
		return option.Options{}, err
	}
	options.Services = append(options.Services, option.Service{
		Type:    C.TypeMultiWAN,
		Tag:     multiwanTag,
		Options: &option.MultiWANServiceOptions{ConfigFile: defaultConfig},
	})
	return options, nil
}

func checkWANPolicies(cfg *config.Config, options option.Options) error {
	check := func(kind, tag string, opts any) error {
		w, ok := opts.(option.DialerOptionsWrapper)
		if !ok {
			return nil
		}
		d := w.TakeDialerOptions()
		if d.WANPolicy == "" {
			return nil
		}
		if _, ok := cfg.PolicyByName(d.WANPolicy); !ok {
			return E.New(kind, "[", tag, "].wan_policy: 未找到 /etc/config/swanix 中的 policy.", d.WANPolicy)
		}
		return nil
	}
	for _, o := range options.Outbounds {
		if err := check("outbounds", o.Tag, o.Options); err != nil {
			return err
		}
	}
	for _, e := range options.Endpoints {
		if err := check("endpoints", e.Tag, e.Options); err != nil {
			return err
		}
	}
	return nil
}

// checkProxy validates proxy.json the same way `run` will load it.
func checkProxy(cfg *config.Config) error {
	_, err := loadProxyOptions(newProxyContext(context.Background()), cfg)
	return err
}

func runProxy(ctx context.Context, cfg *config.Config) error {
	gctx := newProxyContext(ctx)
	options, err := loadProxyOptions(gctx, cfg)
	if err != nil {
		return err
	}
	bctx, cancel := context.WithCancel(service.ExtendContext(gctx))
	defer cancel()
	instance, err := box.New(box.Options{Context: bctx, Options: options})
	if err != nil {
		return E.Cause(err, "create service")
	}
	if err := instance.Start(); err != nil {
		cancel()
		_ = instance.Close()
		return E.Cause(err, "start service")
	}
	<-ctx.Done()
	cancel()
	return instance.Close()
}

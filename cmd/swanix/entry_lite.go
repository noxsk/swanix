//go:build mw_lite

package main

import (
	"context"

	"github.com/sagernet/sing-box/experimental/multiwan/buildinfo"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/daemon"
)

// The Lite entry never constructs box.Box and imports no sing-box protocol,
// option or router package: it runs the shared MultiWAN engine directly.

var profile = config.ProfileLite

func init() { buildinfo.Profile = "lite" }

func proxyFeatures() []string { return nil }

func runDaemon(ctx context.Context, cfg *config.Config) error {
	return daemon.Run(ctx, daemon.Params{Config: cfg, Profile: profile})
}

// checkProxy is unreachable in practice: Validate already rejects proxy.enabled=1
// for the Lite profile. It exists so a future change cannot silently ignore it.
func checkProxy(*config.Config) error {
	return config.Error{Path: "proxy.enabled", Reason: "当前 lite 构建不含代理模块"}
}

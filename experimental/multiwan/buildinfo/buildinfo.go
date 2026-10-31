// Package buildinfo carries version identity and the feature report. The
// variables are set at link time with -X; see ci/build.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

var (
	// Version is the swanix (fork) version.
	Version = "dev"
	// Upstream is the sing-box tag the fork is based on.
	Upstream = "v1.14.4"
	// UpstreamCommit is the full upstream commit of that tag.
	UpstreamCommit = "c005f35300ce76427dc285481ec5695f5b179cd2"
	// Commit is the fork commit this binary was built from.
	Commit = "unknown"
	// Profile is "lite" or "full".
	Profile = "unset"
	// OpenWrt is the OpenWrt series the package was built for (e.g. 24.10, 25.12), if known.
	OpenWrt = ""
	// Verified lists the target systems this build was verified on.
	Verified = ""
)

// Report is printed by `swanix version` and `swanix features`.
type Report struct {
	Version        string   `json:"version"`
	Upstream       string   `json:"upstream_version"`
	UpstreamCommit string   `json:"upstream_commit"`
	Commit         string   `json:"commit"`
	Profile        string   `json:"profile"`
	OpenWrt        string   `json:"openwrt_series,omitempty"`
	GoVersion      string   `json:"go_version"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	CGO            string   `json:"cgo"`
	BuildTags      []string `json:"build_tags"`
	MultiWAN       []string `json:"multiwan_features"`
	Proxy          []string `json:"proxy_features"`
	Verified       []string `json:"verified_scope"`
}

// MultiWANFeatures is shared by both profiles.
var MultiWANFeatures = []string{
	"ipv4-connection-balancing", "health-probes", "dynamic-weights", "policy-routing",
	"router-traffic", "inbound-return-path", "ipv6-native", "ipv6-nat66", "apply-confirm-rollback",
}

// Collect assembles the report. proxy lists the proxy features compiled in
// (empty for Lite).
func Collect(proxy []string) Report {
	r := Report{Version: Version, Upstream: Upstream, UpstreamCommit: UpstreamCommit, Commit: Commit, Profile: Profile,
		OpenWrt: OpenWrt, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		MultiWAN: append([]string{}, MultiWANFeatures...), Proxy: append([]string{}, proxy...), BuildTags: []string{}, Verified: []string{}, CGO: "unknown"}
	sort.Strings(r.Proxy)
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "-tags":
				for _, t := range strings.Split(s.Value, ",") {
					if t != "" {
						r.BuildTags = append(r.BuildTags, t)
					}
				}
			case "CGO_ENABLED":
				r.CGO = s.Value
			}
		}
	}
	sort.Strings(r.BuildTags)
	for _, v := range strings.Split(Verified, ",") {
		if v = strings.TrimSpace(v); v != "" {
			r.Verified = append(r.Verified, v)
		}
	}
	return r
}

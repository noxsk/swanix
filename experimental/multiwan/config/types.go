// Package config defines the standalone swanix MultiWAN configuration model.
//
// It must not import the sing-box option, adapter or router packages: the Lite
// build links this package and must stay free of every proxy dependency.
package config

// CurrentVersion is the configuration structure version (not the program version).
const CurrentVersion = 1

const (
	StartupObserve = "observe"
	StartupManage  = "manage"

	RoleActive = "active"
	RoleBackup = "backup"

	BiasDownload = "download"
	BiasBalanced = "balanced"
	BiasUpload   = "upload"

	AffinityConnection        = "connection"
	AffinitySourceDestination = "source_destination"
	AffinitySource            = "source"

	UnavailableReject = "reject"

	IPv6Native    = "native"
	IPv6NAT66     = "nat66"
	IPv6Unmanaged = "unmanaged"

	OffloadOff      = "off"
	OffloadSoftware = "software"
	OffloadHardware = "hardware"

	TransparentOff    = "off"
	TransparentTUN    = "tun"
	TransparentTProxy = "tproxy"
)

// Config is the whole /etc/config/swanix file.
type Config struct {
	Global   Global
	Kernel   Kernel
	IPv6     IPv6
	Proxy    Proxy
	Health   []Health // named sections
	WANs     []WAN
	Policies []Policy
	Rules    []Rule
}

type Global struct {
	ConfigVersion       int      `uci:"config_version"`
	Enabled             bool     `uci:"enabled"`
	StartupMode         string   `uci:"startup_mode"`
	LANNetworks         []string `uci:"lan_network"`
	ManageRouterTraffic bool     `uci:"manage_router_traffic"`
	DefaultPolicy       string   `uci:"default_policy"`
	FlowOffload         string   `uci:"flow_offload"`
	PersistHistory      bool     `uci:"persist_history"`
	ConfirmTimeout      int      `uci:"confirm_timeout"` // seconds
	UpdateInterval      int      `uci:"update_interval"` // scheduler, seconds
	SampleInterval      int      `uci:"sample_interval"` // byte counters, seconds
}

type Kernel struct {
	MarkMask      string `uci:"mark_mask"`  // e.g. 0xff000000
	TableBase     int    `uci:"table_base"` // routing table = base + id
	PriorityBase  int    `uci:"priority_base"`
	NFTTable      string `uci:"nft_table"`
	ControlSocket string `uci:"control_socket"`
}

type WAN struct {
	Name         string   `uci:"-"` // section name
	ID           int      `uci:"id"`
	Enabled      bool     `uci:"enabled"`
	Network      string   `uci:"network"`
	IPv6Network  string   `uci:"ipv6_network"` // "auto", "", or a logical interface
	DownloadMbps float64  `uci:"download_mbps"`
	UploadMbps   float64  `uci:"upload_mbps"`
	Preference   int      `uci:"preference"`
	Role         string   `uci:"role"`
	TrafficBias  string   `uci:"traffic_bias"`
	HealthRef    string   `uci:"health"`
	ExtraTargets []string `uci:"probe_target"`
}

type Policy struct {
	Name            string   `uci:"-"`
	Members         []string `uci:"member"`
	Affinity        string   `uci:"affinity"`
	AffinityTimeout int      `uci:"affinity_timeout"` // seconds
	// SaturationThreshold (percent of a line's configured capacity, 0 = off): a
	// line at or above it gets no new connections while another member is below it.
	SaturationThreshold int    `uci:"saturation_threshold"`
	UnavailableAction   string `uci:"unavailable_action"`
}

// Rule maps traffic to a policy. Unset match fields match everything.
type Rule struct {
	Name     string   `uci:"-"`
	Enabled  bool     `uci:"enabled"`
	Priority int      `uci:"priority"`
	Src      []string `uci:"src"`
	Dest     []string `uci:"dest"`
	Proto    string   `uci:"proto"`
	DestPort []string `uci:"dest_port"`
	Local    bool     `uci:"local"` // match router-originated traffic only
	Policy   string   `uci:"policy"`
}

type Health struct {
	Name              string   `uci:"-"`
	Targets           []string `uci:"target"` // host:port (tcp) or http(s):// URL
	Interval          int      `uci:"interval"`
	Timeout           int      `uci:"timeout"`
	SuccessesRequired int      `uci:"successes_required"`
	DownRounds        int      `uci:"down_rounds"`
	UpRounds          int      `uci:"up_rounds"`
	RecoverRamp       int      `uci:"recover_ramp"` // seconds
}

type IPv6 struct {
	Mode         string   `uci:"mode"`
	NAT66Router  bool     `uci:"nat66_router_traffic"`
	NAT66Sources []string `uci:"nat66_source"`
}

type Proxy struct {
	Enabled         bool   `uci:"enabled"`
	TransparentMode string `uci:"transparent_mode"`
	ConfigFile      string `uci:"config_file"`
	// InterruptOnDown closes the proxy's wan_policy sockets of a WAN when it goes
	// down, so long-lived sessions redial on a healthy line at once.
	InterruptOnDown bool `uci:"interrupt_on_down"`
}

// Defaults returns a configuration populated with the documented defaults.
// Installation default is disabled and observe-only.
func Defaults() Config {
	return Config{
		Global: Global{
			ConfigVersion:       CurrentVersion,
			StartupMode:         StartupObserve,
			ManageRouterTraffic: true,
			FlowOffload:         OffloadOff,
			ConfirmTimeout:      90,
			UpdateInterval:      10,
			SampleInterval:      2,
		},
		Kernel: Kernel{
			MarkMask:      "0xff000000",
			TableBase:     1000,
			PriorityBase:  10000,
			NFTTable:      "swanix",
			ControlSocket: "/var/run/swanix/control.sock",
		},
		IPv6:  IPv6{Mode: IPv6Native},
		Proxy: Proxy{TransparentMode: TransparentOff, ConfigFile: "/etc/swanix/proxy.json", InterruptOnDown: true},
	}
}

func (w WAN) withDefaults() WAN {
	if w.Preference == 0 {
		w.Preference = 100
	}
	if w.Role == "" {
		w.Role = RoleActive
	}
	if w.TrafficBias == "" {
		w.TrafficBias = BiasBalanced
	}
	if w.IPv6Network == "" {
		w.IPv6Network = "auto"
	}
	return w
}

func (p Policy) withDefaults() Policy {
	if p.Affinity == "" {
		p.Affinity = AffinityConnection
	}
	if p.AffinityTimeout == 0 {
		p.AffinityTimeout = 300
	}
	if p.UnavailableAction == "" {
		p.UnavailableAction = UnavailableReject
	}
	return p
}

func (h Health) withDefaults() Health {
	if len(h.Targets) == 0 {
		h.Targets = []string{"1.1.1.1:443", "9.9.9.9:443", "8.8.8.8:443"}
	}
	if h.Interval == 0 {
		h.Interval = 5
	}
	if h.Timeout == 0 {
		h.Timeout = 3
	}
	if h.SuccessesRequired == 0 {
		h.SuccessesRequired = 1
	}
	if h.DownRounds == 0 {
		h.DownRounds = 3
	}
	if h.UpRounds == 0 {
		h.UpRounds = 3
	}
	if h.RecoverRamp == 0 {
		h.RecoverRamp = 30
	}
	return h
}

// HealthFor returns the effective health profile for a WAN.
func (c *Config) HealthFor(w WAN) Health {
	for _, h := range c.Health {
		if h.Name == w.HealthRef {
			return h
		}
	}
	return Health{}.withDefaults()
}

// WANByName returns the WAN with the given section name.
func (c *Config) WANByName(name string) (WAN, bool) {
	for _, w := range c.WANs {
		if w.Name == name {
			return w, true
		}
	}
	return WAN{}, false
}

// PolicyByName returns the policy with the given section name.
func (c *Config) PolicyByName(name string) (Policy, bool) {
	for _, p := range c.Policies {
		if p.Name == name {
			return p, true
		}
	}
	return Policy{}, false
}

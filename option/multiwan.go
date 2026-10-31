package option

// MultiWANServiceOptions configures the swanix `multiwan` service (Full build).
// The MultiWAN settings themselves live in /etc/config/swanix; this service only
// points at that file so both sides share one source of truth.
type MultiWANServiceOptions struct {
	ConfigFile string `json:"config_file,omitempty"`
}

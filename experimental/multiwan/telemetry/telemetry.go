// Package telemetry holds the bounded, in-memory status model exposed through
// the local control interface. History stays in memory and is cleared on restart;
// global.persist_history is currently reserved and has no effect.
package telemetry

import (
	"sync"
	"time"
)

// Ring is a fixed-size ring buffer.
type Ring[T any] struct {
	mu   sync.Mutex
	buf  []T
	next int
	full bool
}

func NewRing[T any](n int) *Ring[T] { return &Ring[T]{buf: make([]T, n)} }

func (r *Ring[T]) Push(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Snapshot returns the elements oldest first.
func (r *Ring[T]) Snapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]T(nil), r.buf[:r.next]...)
	}
	out := make([]T, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}

// FamilyStatus is the health of one WAN in one address family.
type FamilyStatus struct {
	Family int       `json:"family"`
	State  string    `json:"state"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	RTTms  float64   `json:"rtt_ms"`
	BaseMs float64   `json:"base_rtt_ms"`
	// ProbeFailureRate is the TCP/HTTP probe failure ratio, not link packet loss.
	ProbeFailureRate float64 `json:"probe_failure_rate"`
	// Share is the percent of new connections the scheduler assigns (per policy).
	Share map[string]int `json:"new_connection_share"`
	// SchedulingReason explains workload scoring or evidence fallback by policy.
	SchedulingReason map[string]string `json:"scheduling_reason,omitempty"`
	// Saturated lists policies with saturation pressure and a fresh unsaturated
	// alternative. It includes hysteresis; it does not imply a 1% share or that
	// this tick changed weights. The bounded discount still obeys smoothing.
	Saturated []string `json:"saturated_in,omitempty"`
	Limit     string   `json:"limit_reason,omitempty"`
}

// WANStatus is the status of one WAN.
type WANStatus struct {
	ID           int            `json:"id"`
	Name         string         `json:"name"`
	Network      string         `json:"network"`
	Device       string         `json:"device"`
	DownloadMbps float64        `json:"download_capacity_mbps"`
	UploadMbps   float64        `json:"upload_capacity_mbps"`
	RXMbps       float64        `json:"rx_mbps"`
	TXMbps       float64        `json:"tx_mbps"`
	Assigned     uint64         `json:"assigned_connections"`
	Families     []FamilyStatus `json:"families"`
}

// Transition is a recent state change.
type Transition struct {
	At     time.Time `json:"at"`
	WAN    string    `json:"wan"`
	Family int       `json:"family"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

// HistoryPoint is one 5-second sample of per-WAN throughput.
type HistoryPoint struct {
	At time.Time          `json:"at"`
	RX map[string]float64 `json:"rx_mbps"`
	TX map[string]float64 `json:"tx_mbps"`
}

// InterfaceSample records health independently for each WAN/address family.
// Nil probe metrics mean no fresh probe evidence (including observe mode).
type InterfaceSample struct {
	WAN              string   `json:"wan"`
	Network          string   `json:"network"`
	Device           string   `json:"device"`
	Family           int      `json:"family"`
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
	Managing         bool     `json:"managing"`
	RTTms            *float64 `json:"rtt_ms"`
	ProbeFailureRate *float64 `json:"probe_failure_rate"`
	// DownEvents counts transitions into Down since the preceding sample,
	// including outages that recovered between two samples.
	DownEvents uint64 `json:"down_events"`
}

type InterfaceHistoryPoint struct {
	At         time.Time         `json:"at"`
	Interfaces []InterfaceSample `json:"interfaces"`
}

// InterfaceHistory retains six hours of 30-second health samples in memory.
type InterfaceHistory struct {
	IntervalSeconds  int                     `json:"interval_seconds"`
	RetentionSeconds int                     `json:"retention_seconds"`
	Points           []InterfaceHistoryPoint `json:"points"`
}

// Status is the document returned by `swanix status`.
type Status struct {
	Version     string       `json:"version"`
	Profile     string       `json:"profile"`
	Enabled     bool         `json:"enabled"`
	Mode        string       `json:"mode"`
	Managing    bool         `json:"managing"`
	GeneratedAt time.Time    `json:"generated_at"`
	WANs        []WANStatus  `json:"wans"`
	Rejected    uint64       `json:"rejected_connections"`
	Problems    []string     `json:"problems"`
	Transitions []Transition `json:"recent_transitions"`
}

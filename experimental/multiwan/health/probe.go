package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DialFunc opens a connection that leaves through exactly one WAN. The mark
// selects the probe routing table of that WAN; a failure must never fall over
// to another line, which is guaranteed by the kernel plan, not by this code.
// dev, when set, is the WAN's device: binding to it keeps policy-routing rules
// placed before swanix's (a TUN's auto_route, a VPN) from taking the probe.
type DialFunc func(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error)

// Target is a probe destination. Hosts must be IP literals so name resolution
// can never be answered through a different WAN.
type Target struct {
	Raw    string
	Addr   string // host:port for dialing
	Family int
	URL    *url.URL // set for http/https targets
}

// ParseTarget parses "ip:port", "[ipv6]:port" or an http(s):// URL with an IP host.
func ParseTarget(raw string) (Target, error) {
	t := Target{Raw: raw}
	hostport := raw
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return t, fmt.Errorf("无效探测 URL %q", raw)
		}
		t.URL = u
		hostport = u.Host
		if u.Port() == "" {
			if u.Scheme == "https" {
				hostport = net.JoinHostPort(u.Hostname(), "443")
			} else {
				hostport = net.JoinHostPort(u.Hostname(), "80")
			}
		}
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return t, fmt.Errorf("探测目标 %q 需要 host:port", raw)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return t, fmt.Errorf("探测目标 %q 必须使用 IP 地址，以保证名称解析不经由其他 WAN", raw)
	}
	t.Addr = hostport
	t.Family = 4
	if ip.Is6() {
		t.Family = 6
	}
	return t, nil
}

// DefaultTargets are used for a family when the profile has none of it.
func DefaultTargets(family int) []string {
	if family == 6 {
		return []string{"[2606:4700:4700::1111]:443", "[2620:fe::fe]:443", "[2001:4860:4860::8888]:443"}
	}
	return []string{"1.1.1.1:443", "9.9.9.9:443", "8.8.8.8:443"}
}

// TargetsFor selects the targets of one family.
func TargetsFor(raw []string, family int) ([]Target, error) {
	var out []Target
	for _, r := range raw {
		t, err := ParseTarget(r)
		if err != nil {
			return nil, err
		}
		if t.Family == family {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		for _, r := range DefaultTargets(family) {
			t, _ := ParseTarget(r)
			out = append(out, t)
		}
	}
	return out, nil
}

// TargetResult is one probe of one target.
type TargetResult struct {
	Target string
	OK     bool
	RTT    time.Duration
	Err    string
}

// RoundResult is one completed round across all targets.
type RoundResult struct {
	OK        bool // successes >= required
	Successes int
	Total     int
	RTT       time.Duration // median of successful probes, 0 if none
	Targets   []TargetResult
}

// Prober probes one WAN in one address family.
type Prober struct {
	Family   int
	Mark     uint32
	Device   string // set by the engine before each round; may change (PPPoE)
	Targets  []Target
	Required int
	Timeout  time.Duration
	Dial     DialFunc
}

func (p *Prober) probe(ctx context.Context, t Target) TargetResult {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	res := TargetResult{Target: t.Raw}
	start := time.Now()
	if t.URL == nil {
		c, err := p.Dial(ctx, "tcp", t.Addr, p.Mark, p.Device)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		_ = c.Close()
		res.OK, res.RTT = true, time.Since(start)
		return res
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return p.Dial(ctx, network, t.Addr, p.Mark, p.Device)
		},
		TLSClientConfig:   &tls.Config{ServerName: "", MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: p.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t.URL.String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		res.Err = resp.Status
		return res
	}
	res.OK, res.RTT = true, time.Since(start)
	return res
}

// Round probes all targets in parallel.
func (p *Prober) Round(ctx context.Context) RoundResult {
	results := make([]TargetResult, len(p.Targets))
	var wg sync.WaitGroup
	for i, t := range p.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.probe(ctx, t)
		}()
	}
	wg.Wait()
	rr := RoundResult{Total: len(results), Targets: results}
	var rtts []time.Duration
	for _, r := range results {
		if r.OK {
			rr.Successes++
			rtts = append(rtts, r.RTT)
		}
	}
	if len(rtts) > 0 {
		sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
		rr.RTT = rtts[len(rtts)/2]
	}
	rr.OK = rr.Successes >= p.Required
	return rr
}

// Quality tracks a rolling window of probe outcomes. LossRate is the TCP/HTTP
// probe failure ratio and must not be presented as link packet loss.
type Quality struct {
	window int
	oks    []bool
	rtts   []time.Duration
}

func NewQuality(window int) *Quality { return &Quality{window: window} }

func (q *Quality) Push(r RoundResult) {
	for _, t := range r.Targets {
		q.oks = append(q.oks, t.OK)
		if t.OK {
			q.rtts = append(q.rtts, t.RTT)
		}
	}
	if n := len(q.oks) - q.window*4; n > 0 {
		q.oks = q.oks[n:]
	}
	if n := len(q.rtts) - q.window; n > 0 {
		q.rtts = q.rtts[n:]
	}
}

// LossRate is the failure ratio of recent probes.
func (q *Quality) LossRate() float64 {
	if len(q.oks) == 0 {
		return 0
	}
	bad := 0
	for _, ok := range q.oks {
		if !ok {
			bad++
		}
	}
	return float64(bad) / float64(len(q.oks))
}

// BaseRTT is the smallest recent RTT, the line's idle baseline.
func (q *Quality) BaseRTT() time.Duration {
	var min time.Duration
	for _, r := range q.rtts {
		if min == 0 || r < min {
			min = r
		}
	}
	return min
}

// Lossy reports a sustained probe-failure ratio. The ratio is not link packet loss.
func (q *Quality) Lossy() bool {
	return len(q.oks) >= 8 && q.LossRate() > 0.3
}

// Slow reports a probe much slower than this line's own idle baseline.
func (q *Quality) Slow(rtt time.Duration) bool {
	base := q.BaseRTT()
	return base > 0 && rtt > 3*base+50*time.Millisecond
}

// Degraded flags a usable but impaired line, before load is taken into account.
func (q *Quality) Degraded(rtt time.Duration) bool {
	return q.Lossy() || q.Slow(rtt)
}

// loadUtilisation is the used fraction of capacity at which probe RTT includes
// this line's own queue. That delay is the transfer, not a worse path.
const loadUtilisation = 0.2

// QueueingDelay reports that rx/tx already explain a high probe RTT.
// Rates are the greater of the smoothed rate and the last sample interval.
func QueueingDelay(downMbps, upMbps, rxMbps, txMbps float64) bool {
	u := 0.0
	if downMbps > 0 {
		u = rxMbps / downMbps
	}
	if upMbps > 0 && txMbps/upMbps > u {
		u = txMbps / upMbps
	}
	return u >= loadUtilisation
}

// Impaired is the quality signal for the state machine. Probe loss always
// counts. Extra delay counts only while the line is idle; delay while it is
// forwarding is queueing caused by the traffic just assigned to it.
func (q *Quality) Impaired(rtt time.Duration, loaded bool) bool {
	if q.Lossy() {
		return true
	}
	if loaded {
		return false
	}
	return q.Slow(rtt)
}

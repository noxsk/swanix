// Command perfload generates and serves test load for the swanix performance tests
// (cmd/swanix/e2e_perf_test.go). It is a test fixture, not a benchmark suite of its own.
//
//	perfload serve   -listen 203.0.113.10:9000 -banner wan1
//	perfload connect -target 203.0.113.10:9000 -c 32 -d 8s [-src a,b,...]
//	perfload bulk    -target 203.0.113.10:9000 -streams 8 -d 8s [-src a,b,...]
//
// The server answers every connection with its banner line (the name of the line that
// carried it) and, if the client then sends "S", streams data until the client closes.
// connect opens connections as fast as its workers can (connect, read banner, reset)
// and reports the rate, latency percentiles and the banner of every answer; bulk runs
// parallel downloads and reports the throughput per banner. Results are one JSON object.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: perfload serve|connect|bulk [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "connect":
		err = connect(os.Args[2:])
	case "bulk":
		err = bulk(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfload:", err)
		os.Exit(1)
	}
}

var chunk = make([]byte, 64<<10)

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "address:port")
	banner := fs.String("banner", "", "name sent to every client")
	_ = fs.Parse(args)
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		_ = c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) })
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp", *listen)
	if err != nil {
		return err
	}
	line := []byte(*banner + "\n")
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			if _, err := c.Write(line); err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b := make([]byte, 1)
			if n, _ := c.Read(b); n == 1 && b[0] == 'S' {
				_ = c.SetReadDeadline(time.Time{})
				for {
					if _, err := c.Write(chunk); err != nil {
						return
					}
				}
			}
		}()
	}
}

type sources struct {
	addrs []*net.TCPAddr
	n     atomic.Uint64
}

func newSources(list string) (*sources, error) {
	s := &sources{}
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		ip := net.ParseIP(a)
		if ip == nil {
			return nil, fmt.Errorf("bad source %q", a)
		}
		s.addrs = append(s.addrs, &net.TCPAddr{IP: ip})
	}
	return s, nil
}

func (s *sources) next() *net.TCPAddr {
	if len(s.addrs) == 0 {
		return nil
	}
	return s.addrs[s.n.Add(1)%uint64(len(s.addrs))]
}

var dialTimeout = 3 * time.Second

func dialer(src *net.TCPAddr) *net.Dialer {
	d := &net.Dialer{Timeout: dialTimeout}
	if src != nil {
		d.LocalAddr = src
	}
	return d
}

func errKind(err error) string {
	var se syscall.Errno
	switch {
	case errors.As(err, &se):
		return se.Error()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "eof"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "other"
}

type connectResult struct {
	Mode        string         `json:"mode"`
	Concurrency int            `json:"concurrency"`
	Seconds     float64        `json:"seconds"`
	OK          int            `json:"ok"`
	Errors      map[string]int `json:"errors"`
	PerLine     map[string]int `json:"per_line"`
	CPS         float64        `json:"connections_per_second"`
	P50ms       float64        `json:"latency_p50_ms"`
	P90ms       float64        `json:"latency_p90_ms"`
	P99ms       float64        `json:"latency_p99_ms"`
	Maxms       float64        `json:"latency_max_ms"`
}

// connect: each worker connects, reads the banner and resets the connection (no
// TIME_WAIT on the client), as fast as it can until the duration is over.
func connect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	target := fs.String("target", "", "address:port")
	conc := fs.Int("c", 32, "concurrent workers")
	dur := fs.Duration("d", 8*time.Second, "duration")
	srcList := fs.String("src", "", "comma-separated source addresses, used round-robin")
	timeout := fs.Duration("timeout", 3*time.Second, "connect and banner timeout")
	_ = fs.Parse(args)
	dialTimeout = *timeout
	src, err := newSources(*srcList)
	if err != nil {
		return err
	}
	type acc struct {
		lat     []time.Duration
		errs    map[string]int
		perLine map[string]int
	}
	accs := make([]acc, *conc)
	start := time.Now()
	end := start.Add(*dur)
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func(a *acc) {
			defer wg.Done()
			a.errs, a.perLine = map[string]int{}, map[string]int{}
			for time.Now().Before(end) {
				t0 := time.Now()
				c, err := dialer(src.next()).Dial("tcp", *target)
				if err != nil {
					a.errs[errKind(err)]++
					continue
				}
				_ = c.SetReadDeadline(time.Now().Add(dialTimeout))
				line, err := bufio.NewReaderSize(c, 64).ReadString('\n')
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = c.Close()
				if err != nil {
					a.errs[errKind(err)]++
					continue
				}
				a.lat = append(a.lat, time.Since(t0))
				a.perLine[strings.TrimSpace(line)]++
			}
		}(&accs[w])
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	res := connectResult{Mode: "connect", Concurrency: *conc, Seconds: secs, Errors: map[string]int{}, PerLine: map[string]int{}}
	var lat []time.Duration
	for _, a := range accs {
		lat = append(lat, a.lat...)
		for k, v := range a.errs {
			res.Errors[k] += v
		}
		for k, v := range a.perLine {
			res.PerLine[k] += v
		}
	}
	res.OK = len(lat)
	res.CPS = float64(res.OK) / secs
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		pct := func(p float64) float64 {
			return float64(lat[int(p*float64(len(lat)-1))]) / float64(time.Millisecond)
		}
		res.P50ms, res.P90ms, res.P99ms, res.Maxms = pct(0.50), pct(0.90), pct(0.99), pct(1)
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

type bulkResult struct {
	Mode       string             `json:"mode"`
	Streams    int                `json:"streams"`
	Seconds    float64            `json:"seconds"`
	Gbps       float64            `json:"gbit_per_second"`
	PerLineMB  map[string]float64 `json:"per_line_mbytes"`
	Errors     map[string]int     `json:"errors"`
	StreamsMin float64            `json:"slowest_stream_gbit_per_second"`
}

// bulk: parallel downloads for the duration; throughput is measured on the client.
func bulk(args []string) error {
	fs := flag.NewFlagSet("bulk", flag.ExitOnError)
	target := fs.String("target", "", "address:port")
	streams := fs.Int("streams", 8, "parallel downloads")
	dur := fs.Duration("d", 8*time.Second, "duration")
	srcList := fs.String("src", "", "comma-separated source addresses, used round-robin")
	_ = fs.Parse(args)
	src, err := newSources(*srcList)
	if err != nil {
		return err
	}
	type acc struct {
		bytes int64
		line  string
		err   string
	}
	accs := make([]acc, *streams)
	start := time.Now()
	end := start.Add(*dur)
	var wg sync.WaitGroup
	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func(a *acc) {
			defer wg.Done()
			c, err := dialer(src.next()).Dial("tcp", *target)
			if err != nil {
				a.err = errKind(err)
				return
			}
			defer c.Close()
			r := bufio.NewReaderSize(c, 256<<10)
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, err := r.ReadString('\n')
			if err != nil {
				a.err = errKind(err)
				return
			}
			a.line = strings.TrimSpace(line)
			if _, err := c.Write([]byte("S")); err != nil {
				a.err = errKind(err)
				return
			}
			_ = c.SetReadDeadline(end)
			buf := make([]byte, 256<<10)
			for {
				n, err := r.Read(buf)
				a.bytes += int64(n)
				if err != nil {
					if !errors.Is(err, os.ErrDeadlineExceeded) {
						a.err = errKind(err)
					}
					return
				}
			}
		}(&accs[i])
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	res := bulkResult{Mode: "bulk", Streams: *streams, Seconds: secs, PerLineMB: map[string]float64{}, Errors: map[string]int{}}
	var total int64
	res.StreamsMin = -1
	for _, a := range accs {
		total += a.bytes
		if a.line != "" {
			res.PerLineMB[a.line] += float64(a.bytes) / 1e6
		}
		if a.err != "" {
			res.Errors[a.err]++
		}
		g := float64(a.bytes) * 8 / secs / 1e9
		if res.StreamsMin < 0 || g < res.StreamsMin {
			res.StreamsMin = g
		}
	}
	res.Gbps = float64(total) * 8 / secs / 1e9
	return json.NewEncoder(os.Stdout).Encode(res)
}

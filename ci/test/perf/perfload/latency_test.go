package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConnectLatencyPercentiles(t *testing.T) {
	// Reversed input ensures the quantiles are computed after sorting.
	lat := make([]time.Duration, 101)
	for i := range lat {
		lat[i] = time.Duration(101-i) * time.Millisecond
	}
	var r connectResult
	r.setLatencies(lat)
	if r.P50ms != 51 || r.P90ms != 91 || r.P95ms != 96 || r.P99ms != 100 || r.Maxms != 101 {
		t.Fatalf("unexpected empirical quantiles: %+v", r)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["latency_p95_ms"] != float64(96) {
		t.Fatalf("missing or wrong p95 wire field: %s", data)
	}
}

func TestConnectLatencySmallSamples(t *testing.T) {
	for _, lat := range [][]time.Duration{nil, {1500 * time.Microsecond}, {time.Millisecond, 2 * time.Millisecond}} {
		var r connectResult
		r.setLatencies(lat)
		if len(lat) == 0 {
			if r.P95ms != 0 || r.Maxms != 0 {
				t.Fatal(r)
			}
		} else if r.P95ms != float64(lat[0])/float64(time.Millisecond) || r.Maxms != float64(lat[len(lat)-1])/float64(time.Millisecond) {
			t.Fatal(r)
		}
	}
}

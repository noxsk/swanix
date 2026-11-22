package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestPolicyObjectiveDefaults(t *testing.T) {
	p := (Policy{}).withDefaults()
	if p.Objective != ObjectiveCapacity || p.FlowSizeKiB != 256 || p.LatencyReservePercent != 0 {
		t.Fatalf("defaults: %+v", p)
	}
	cfg, err := Parse("config policy 'test'\n")
	if err != nil {
		t.Fatal(err)
	}
	p = cfg.Policies[0]
	if p.Objective != ObjectiveCapacity || p.FlowSizeKiB != 256 {
		t.Fatalf("UCI defaults: %+v", p)
	}
}

func TestPolicyObjectives(t *testing.T) {
	for _, objective := range []string{ObjectiveCapacity, ObjectiveLatency, ObjectiveThroughput, ObjectiveBalanced} {
		t.Run(objective, func(t *testing.T) {
			for _, size := range []int{1, 256, 1048576} {
				reserve := 0
				if objective == ObjectiveThroughput {
					reserve = 80
				}
				cfg, err := LoadFile(samplePath)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := Parse(fmt.Sprintf("config policy 'test'\n option objective '%s'\n option flow_size_kib '%d'\n option latency_reserve_percent '%d'\n", objective, size, reserve))
				if err != nil {
					t.Fatal(err)
				}
				p := parsed.Policies[0]
				if p.Objective != objective || p.FlowSizeKiB != size || p.LatencyReservePercent != reserve {
					t.Fatalf("decode: %+v", p)
				}
				p.Name, p.Members = cfg.Policies[0].Name, cfg.Policies[0].Members
				cfg.Policies[0] = p
				if err := cfg.Validate(ProfileLite); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPolicyObjectiveRejections(t *testing.T) {
	for _, tc := range []struct{ option, value string }{
		{"objective", "unknown"}, {"objective", ""},
		{"flow_size_kib", "0"}, {"flow_size_kib", "-1"}, {"flow_size_kib", "1048577"},
		{"flow_size_kib", "1.5"}, {"latency_reserve_percent", "-1"},
		{"latency_reserve_percent", "81"}, {"latency_reserve_percent", "0.5"},
	} {
		t.Run(tc.option+"="+tc.value, func(t *testing.T) {
			err := mutate(t, "option unavailable_action 'reject'", fmt.Sprintf("option unavailable_action 'reject'\n option %s '%s'", tc.option, tc.value))
			if err == nil || !strings.Contains(err.Error(), "."+tc.option) {
				t.Fatalf("expected %s error, got %v", tc.option, err)
			}
		})
	}
	for _, objective := range []string{ObjectiveCapacity, ObjectiveLatency, ObjectiveBalanced} {
		t.Run(objective+" reserve", func(t *testing.T) {
			err := mutate(t, "option unavailable_action 'reject'", fmt.Sprintf("option unavailable_action 'reject'\n option objective '%s'\n option latency_reserve_percent '1'", objective))
			if err == nil || !strings.Contains(err.Error(), ".latency_reserve_percent") {
				t.Fatalf("expected reserve rejection, got %v", err)
			}
		})
	}
}

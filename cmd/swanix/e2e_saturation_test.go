package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// TestLiteSaturationSpillover: both lines are configured as 4 Mbit/s and wan1
// gets 20 Mbit/s of traffic; with saturation_threshold 80 its new connections go to
// wan2 at once, and come back once the load is gone.
func TestLiteSaturationSpillover(t *testing.T) {
	nstest.Require(t)
	tp := nstest.New(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "swanix")
	build := exec.Command("go", "build", "-tags", "mw_lite", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	cfg := strings.ReplaceAll(e2eConfig, "option download_mbps '100'", "option download_mbps '4'") // both lines 4 Mbit/s
	cfg = strings.Replace(cfg, "\tlist member 'wan2'\n", "\tlist member 'wan2'\n\toption saturation_threshold '80'\n", 1)
	cfgPath := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWANIX_CONFIG", cfgPath)
	if out, err := exec.Command(bin, "check").CombinedOutput(); err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	var logs bytes.Buffer
	daemon := exec.Command(bin, "run")
	daemon.Stdout, daemon.Stderr = &logs, &logs
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = daemon.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGKILL)
		<-done
		if t.Failed() {
			t.Logf("daemon log:\n%s", logs.String())
		}
	})
	waitFor(t, "both WANs healthy", 20*time.Second, func() bool {
		st, ok := status(t, bin)
		if !ok {
			return false
		}
		a, sa := state(st, "wan1")
		b, sb := state(st, "wan2")
		return a == "healthy" && b == "healthy" && sa > 0 && sb > 0
	})
	saturated := func() bool {
		st, _ := status(t, bin)
		for _, w := range st.WANs {
			if w.Name == "wan1" {
				for _, s := range w.Families[0].Saturated {
					if s == "balanced" {
						return true
					}
				}
			}
		}
		return false
	}
	if r := tp.Count("lan", 60); r["wan1"] < 5 {
		t.Fatalf("before the load both lines should take connections: %v", r)
	}

	// about 20 Mbit/s arrives on wan1 (UDP to the router, dropped there): 5x its configured capacity
	load := exec.Command("ip", "netns", "exec", "isp1", "python3", "-I", "-c", `
import socket,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); b=b"x"*1250; end=time.time()+25
while time.time()<end:
    t=time.time()
    for i in range(20): s.sendto(b,("192.0.2.2",9))
    time.sleep(max(0,0.01-(time.time()-t)))`)
	if err := load.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = load.Process.Kill(); _ = load.Wait() }()
	waitFor(t, "wan1 reported saturated", 15*time.Second, saturated)
	r := tp.Count("lan", 100)
	if r["wan1"]+r["wan2"] != 100 || r["wan1"] > 5 {
		t.Fatalf("new connections should leave the saturated line: %v", r)
	}
	t.Logf("while wan1 is saturated: %v", r)

	_ = load.Process.Kill()
	_ = load.Wait()
	waitFor(t, "wan1 released", 20*time.Second, func() bool { return !saturated() })
	waitFor(t, "wan1 takes connections again", 40*time.Second, func() bool {
		st, _ := status(t, bin)
		_, share := state(st, "wan1")
		return share >= 20
	})
	if !strings.Contains(logs.String(), "saturated in policy balanced") {
		t.Fatalf("saturation not logged")
	}
}

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// TestMeasureResources reports idle memory and CPU of released binaries. It is a
// measurement, not a gate, and only runs when SWANIX_MEASURE_LITE / SWANIX_MEASURE_FULL
// point at binaries (for example the CI artifacts). Numbers come from one run on
// whatever machine executes it and say nothing about routers.
func TestMeasureResources(t *testing.T) {
	nstest.Require(t)
	lite, full := os.Getenv("SWANIX_MEASURE_LITE"), os.Getenv("SWANIX_MEASURE_FULL")
	if lite == "" && full == "" {
		t.Skip("set SWANIX_MEASURE_LITE and/or SWANIX_MEASURE_FULL")
	}
	tp := nstest.New(t)
	dir := t.TempDir()
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})

	proxyPath := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(proxyPath, []byte(proxyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, bin, extra string }{}
	if lite != "" {
		cases = append(cases, struct{ name, bin, extra string }{"lite", lite, ""})
	}
	if full != "" {
		cases = append(cases,
			struct{ name, bin, extra string }{"full, proxy off", full, ""},
			struct{ name, bin, extra string }{"full, proxy on (socks + direct)", full,
				"config proxy 'proxy'\n\toption enabled '1'\n\toption config_file '" + proxyPath + "'\n\n"})
	}

	clk := float64(100) // CLK_TCK; stat fields are in ticks
	for _, c := range cases {
		cfg := strings.Replace(e2eConfig, "config ipv6 'ipv6'", c.extra+"config ipv6 'ipv6'", 1)
		cfgPath := filepath.Join(dir, "swanix.conf")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(c.bin, "run")
		cmd.Env = append(os.Environ(), "SWANIX_CONFIG="+cfgPath)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		// wait for both lines to be probed healthy
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			out, err := exec.Command(c.bin, "status").CombinedOutput()
			if err == nil && strings.Count(string(out), "healthy") >= 2 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(5 * time.Second)
		cpu0 := cpuTicks(t, pid)
		time.Sleep(30 * time.Second)
		idleCPU := (cpuTicks(t, pid) - cpu0) / clk
		rss, pss := memKB(t, pid)
		// a burst of connections through the router
		cpu1 := cpuTicks(t, pid)
		tp.Count("lan", 1500)
		burstCPU := (cpuTicks(t, pid) - cpu1) / clk
		rss2, _ := memKB(t, pid)
		t.Logf("MEASURE %-34s idle: RSS %6.1f MiB  PSS %6.1f MiB  CPU %.2f s over 30 s | after 1500 connections: RSS %6.1f MiB, CPU +%.2f s",
			c.name, float64(rss)/1024, float64(pss)/1024, idleCPU, float64(rss2)/1024, burstCPU)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		_ = exec.Command(c.bin, "remove").Run()
	}
}

func cpuTicks(t *testing.T, pid int) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	u, _ := strconv.ParseFloat(f[11], 64)
	k, _ := strconv.ParseFloat(f[12], 64)
	return u + k
}

func memKB(t *testing.T, pid int) (rss, pss int) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		var v int
		if strings.HasPrefix(l, "Rss:") {
			fmt.Sscanf(l, "Rss: %d kB", &v)
			rss = v
		} else if strings.HasPrefix(l, "Pss:") {
			fmt.Sscanf(l, "Pss: %d kB", &v)
			pss = v
		}
	}
	return
}

const bulkServerPy = `
import socket,sys,threading
host,port,total=sys.argv[1],int(sys.argv[2]),int(sys.argv[3])
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind((host,port)); s.listen(16)
chunk=b"\0"*(1<<20)
def h(c):
    try:
        c.recv(1)
        left=total
        while left>0:
            n=min(left,len(chunk)); c.sendall(chunk[:n]); left-=n
    except Exception: pass
    c.close()
while True:
    c,_=s.accept(); threading.Thread(target=h,args=(c,),daemon=True).start()
`

const bulkClientPy = `
import socket,sys,time
mode,total=sys.argv[1],int(sys.argv[2])
s=socket.socket()
if mode=="socks":
    s.connect(("192.168.1.1",1080)); s.sendall(b"\x05\x01\x00"); assert s.recv(2)==b"\x05\x00"
    s.sendall(b"\x05\x01\x00\x01"+bytes([203,0,113,10])+(5001).to_bytes(2,"big")); assert s.recv(10)[1]==0
else:
    s.connect(("203.0.113.10",5001))
t=time.time(); s.sendall(b"x"); got=0
while got<total:
    d=s.recv(1<<20)
    if not d: break
    got+=len(d)
dt=time.time()-t
print("%.1f" % (got*8/dt/1e6))
`

// TestMeasureThroughput compares one bulk TCP download through the kernel path (the daemon is
// not in the data path) with the same download through the proxy runtime (SOCKS + direct),
// and reports the daemon's CPU time for each. One run, loopback-class veth links in a micro-VM:
// it shows the relative cost of putting traffic through user space, nothing about routers.
func TestMeasureThroughput(t *testing.T) {
	nstest.Require(t)
	full := os.Getenv("SWANIX_MEASURE_FULL")
	if full == "" {
		t.Skip("set SWANIX_MEASURE_FULL")
	}
	_ = nstest.New(t)
	dir := t.TempDir()
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	proxyPath := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(proxyPath, []byte(strings.Replace(proxyJSON, `"wan_policy": "balanced"`, `"wan_policy": "balanced"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	const total = 1 << 30
	srv := exec.Command("ip", "netns", "exec", "isp1", "python3", "-I", "-c", bulkServerPy, "203.0.113.10", "5001", fmt.Sprint(total))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	// make wan1 the only usable line so the bulk server (isp1) is the one answering
	cfg := strings.Replace(e2eConfig, "config wan 'wan2'\n\toption id '2'", "config wan 'wan2'\n\toption id '2'\n\toption role 'backup'", 1)
	cfg = strings.Replace(cfg, "config ipv6 'ipv6'", "config proxy 'proxy'\n\toption enabled '1'\n\toption config_file '"+proxyPath+"'\n\nconfig ipv6 'ipv6'", 1)
	cfgPath := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWANIX_CONFIG", cfgPath)
	d := exec.Command(full, "run")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Signal(syscall.SIGTERM); _ = d.Wait(); _ = exec.Command(full, "remove").Run() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := exec.Command(full, "status").CombinedOutput(); err == nil && strings.Count(string(out), "healthy") >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	for _, mode := range []string{"kernel", "socks", "kernel", "socks"} {
		cpu0 := cpuTicks(t, d.Process.Pid)
		out := nstest.NS(t, "lan", "python3", "-I", "-c", bulkClientPy, mode, fmt.Sprint(total))
		cpu := (cpuTicks(t, d.Process.Pid) - cpu0) / 100
		t.Logf("MEASURE bulk download 1 GiB via %-6s: %s Mbit/s, daemon CPU %.2f s", mode, strings.TrimSpace(out), cpu)
	}
}

//go:build !mw_lite

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

const socksClientPy = `
import socket,sys,json,collections,errno
n=int(sys.argv[1]); res=collections.Counter()
for i in range(n):
    s=socket.socket(); s.settimeout(3)
    try:
        s.connect(("192.168.1.1",1080))
        s.sendall(b"\x05\x01\x00"); assert s.recv(2)==b"\x05\x00"
        s.sendall(b"\x05\x01\x00\x01"+bytes([203,0,113,10])+(80).to_bytes(2,"big"))
        rep=s.recv(10)
        if rep[1]!=0: res["err:socks%d"%rep[1]]+=1; continue
        res[s.recv(64).decode().strip()]+=1
    except Exception as e:
        res["err:"+type(e).__name__]+=1
    finally: s.close()
print(json.dumps(res))
`

// syncBuffer collects the daemon's output while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// socksHoldPy opens SOCKS sessions to the banner server until one leaves through
// wan2, prints "ready", then waits on it: "closed" when the proxy ends it.
const socksHoldPy = `
import socket,sys,time
for i in range(50):
    s=socket.socket(); s.settimeout(3)
    s.connect(("192.168.1.1",1080))
    s.sendall(b"\x05\x01\x00"); s.recv(2)
    s.sendall(b"\x05\x01\x00\x01"+bytes([203,0,113,10])+(80).to_bytes(2,"big")); s.recv(10)
    if s.recv(64).decode().strip()=="wan2": break
    s.close()
else:
    print("no session on wan2"); sys.exit(1)
print("ready")
s.settimeout(120)
try:
    time.sleep(0.5); s.sendall(b"x")
    d=s.recv(64)
    print("closed (eof)" if not d else "answered %r" % d)
except OSError as e:
    print("closed (%s)" % type(e).__name__)
`

// TestFullProxyUsesWANPolicy runs the Full binary with proxy.enabled=1: the
// sing-box runtime hosts the multiwan service, and a direct outbound with
// wan_policy picks the WAN per connection right before connect.
func TestFullProxyUsesWANPolicy(t *testing.T) {
	nstest.Require(t)
	tp := nstest.New(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "swanix")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	proxyPath := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(proxyPath, []byte(proxyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(e2eConfig, "config ipv6 'ipv6'", "config proxy 'proxy'\n\toption enabled '1'\n\toption config_file '"+proxyPath+"'\n\nconfig ipv6 'ipv6'", 1)
	cfgPath := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWANIX_CONFIG", cfgPath)

	if out, err := exec.Command(bin, "check").CombinedOutput(); err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}

	// a wan_policy that does not exist must be caught before anything starts
	bad := strings.Replace(proxyJSON, `"wan_policy": "balanced"`, `"wan_policy": "nope"`, 1)
	if err := os.WriteFile(proxyPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "check").CombinedOutput(); err == nil || !strings.Contains(string(out), "wan_policy") {
		t.Fatalf("unknown policy not rejected: %v %s", err, out)
	}
	// wan_policy together with detour is rejected by the dialer
	if err := os.WriteFile(proxyPath, []byte(proxyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs syncBuffer
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

	waitFor(t, "both WANs healthy through the Full runtime", 30*time.Second, func() bool {
		st, ok := status(t, bin)
		if !ok {
			return false
		}
		a, sa := state(st, "wan1")
		b, sb := state(st, "wan2")
		return a == "healthy" && b == "healthy" && sa > 0 && sb > 0
	})

	count := func(n string) map[string]int {
		out := nstest.NS(t, "lan", "python3", "-I", "-c", socksClientPy, n)
		return parseCounts(t, out)
	}

	t.Run("proxy connections are balanced per connection", func(t *testing.T) {
		r := count("200")
		if r["wan1"]+r["wan2"] != 200 {
			t.Fatalf("lost: %v", r)
		}
		nstest.Within(t, "wan1 share", r["wan1"], 200, 0.50, 0.15)
	})

	t.Run("a proxy session on a line that dies is closed, not left hanging", func(t *testing.T) {
		// hold one SOCKS session that left through wan2, then blackhole isp2: without
		// interrupt_on_down the session would hang until TCP gives up (minutes)
		cmd := exec.Command("ip", "netns", "exec", "lan", "python3", "-I", "-u", "-c", socksHoldPy)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		lines := make(chan string, 4)
		go func() {
			buf := make([]byte, 256)
			var acc string
			for {
				n, err := stdout.Read(buf)
				acc += string(buf[:n])
				for strings.Contains(acc, "\n") {
					i := strings.Index(acc, "\n")
					lines <- acc[:i]
					acc = acc[i+1:]
				}
				if err != nil {
					close(lines)
					return
				}
			}
		}()
		if l := <-lines; l != "ready" {
			t.Fatalf("could not hold a session on wan2: %q", l)
		}
		nstest.NS(t, "isp2", "nft", "add", "table", "inet", "blackhole")
		nstest.NS(t, "isp2", "nft", "add", "chain", "inet", "blackhole", "in", "{ type filter hook input priority 0; policy drop; }")
		t.Cleanup(func() {
			_ = exec.Command("ip", "netns", "exec", "isp2", "nft", "delete", "table", "inet", "blackhole").Run()
		})
		start := time.Now()
		select {
		case l := <-lines:
			if !strings.HasPrefix(l, "closed") {
				t.Fatalf("session on the dead line: %q", l)
			}
			t.Logf("session %s, %v after the line went dark", l, time.Since(start).Round(time.Second))
		case <-time.After(60 * time.Second):
			t.Fatal("the session on the dead line is still open after 60 s")
		}
		waitFor(t, "the interrupt in the daemon log", 5*time.Second, func() bool {
			return strings.Contains(logs.String(), "proxy connection(s) dialled through it")
		})
		_ = exec.Command("ip", "netns", "exec", "isp2", "nft", "delete", "table", "inet", "blackhole").Run()
		waitFor(t, "wan2 healthy again", 40*time.Second, func() bool {
			st, _ := status(t, bin)
			s, _ := state(st, "wan2")
			return s == "healthy"
		})
	})

	t.Run("proxy follows failover", func(t *testing.T) {
		tp.StopServer("isp2", "203.0.113.10")
		waitFor(t, "wan2 out of the pool", 25*time.Second, func() bool {
			st, _ := status(t, bin)
			s, share := state(st, "wan2")
			return s == "down" && share == 0
		})
		if r := count("30"); r["wan1"] != 30 {
			t.Fatalf("proxy still used the failed line: %v", r)
		}
	})
}

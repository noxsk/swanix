// Package nstest builds the two-ISP test topology out of network namespaces.
// It is only for tests and must run inside a disposable namespace (see
// ci/test/netns.sh); it refuses to run otherwise.
//
//	lan(192.168.1.100) -- br-lan [router] eth1 -- isp1(192.0.2.1, 203.0.113.10)
//	                                      eth2 -- isp2(198.51.100.1, 203.0.113.10)
//
// Both ISPs serve the same "internet" address 203.0.113.10 and answer with
// their own name, so the answer shows which WAN carried a connection.
package nstest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const serverPy = `
import socket,sys,threading
banner=sys.argv[1].encode(); host=sys.argv[2]; port=int(sys.argv[3])
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind((host,port)); s.listen(512)
def h(c):
    try:
        c.sendall(banner+b"\n")
        while True:
            d=c.recv(16)
            if not d: break
            c.sendall(banner+b"\n")
    except Exception: pass
    c.close()
while True:
    c,_=s.accept(); threading.Thread(target=h,args=(c,),daemon=True).start()
`

const clientPy = `
import socket,sys,json,errno,collections
host,port,n=sys.argv[1],int(sys.argv[2]),int(sys.argv[3])
mark=int(sys.argv[4]) if len(sys.argv)>4 else 0
bind=sys.argv[5] if len(sys.argv)>5 else ""
res=collections.Counter()
for i in range(n):
    s=socket.socket(); s.settimeout(2)
    try:
        if mark: s.setsockopt(socket.SOL_SOCKET,36,mark)
        if bind: s.bind((bind,0))
        s.connect((host,port)); res[s.recv(64).decode().strip()]+=1
    except OSError as e:
        res["err:"+errno.errorcode.get(e.errno or 0,"timeout")]+=1
    finally: s.close()
print(json.dumps(res))
`

// Topo is a built topology.
type Topo struct {
	T       *testing.T
	servers map[string]*exec.Cmd
}

// Require skips unless running in a disposable namespace.
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv("SWANIX_NETNS") != "1" {
		t.Skip("set SWANIX_NETNS=1 and run inside `unshare -n` (ci/test/netns.sh)")
	}
}

// Sh runs a command and fails the test on error.
func Sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// NS runs a command inside a named namespace.
func NS(t *testing.T, ns string, args ...string) string {
	t.Helper()
	return Sh(t, "ip", append([]string{"netns", "exec", ns}, args...)...)
}

// New builds the topology. strictRP leaves strict rp_filter on the WAN devices.
func New(t *testing.T) *Topo {
	t.Helper()
	Require(t)
	tp := &Topo{T: t, servers: map[string]*exec.Cmd{}}
	t.Cleanup(func() {
		for _, c := range tp.servers {
			_ = c.Process.Signal(syscall.SIGKILL)
			_ = c.Wait()
		}
		// leave the namespace clean so several tests can run in one invocation
		for _, ns := range []string{"lan", "isp1", "isp2"} {
			_ = exec.Command("ip", "netns", "del", ns).Run()
		}
		for _, l := range []string{"br-lan", "eth1", "eth2"} {
			_ = exec.Command("ip", "link", "del", l).Run()
		}
		_ = exec.Command("nft", "delete", "table", "inet", "fw4sim").Run()
	})
	for _, ns := range []string{"lan", "isp1", "isp2"} {
		Sh(t, "ip", "netns", "add", ns)
		NS(t, ns, "ip", "link", "set", "lo", "up")
	}
	Sh(t, "ip", "link", "set", "lo", "up")
	link := func(local, ns, peer string) {
		Sh(t, "ip", "link", "add", local, "type", "veth", "peer", "name", peer, "netns", ns)
	}
	link("br-lan", "lan", "eth0")
	link("eth1", "isp1", "wan0")
	link("eth2", "isp2", "wan0")
	addr := func(ns, dev, cidr string) {
		if ns == "" {
			Sh(t, "ip", "addr", "add", cidr, "dev", dev)
			Sh(t, "ip", "link", "set", dev, "up")
			return
		}
		NS(t, ns, "ip", "addr", "add", cidr, "dev", dev)
		NS(t, ns, "ip", "link", "set", dev, "up")
	}
	addr("", "br-lan", "192.168.1.1/24")
	addr("lan", "eth0", "192.168.1.100/24")
	NS(t, "lan", "ip", "route", "add", "default", "via", "192.168.1.1")
	addr("", "eth1", "192.0.2.2/24")
	addr("isp1", "wan0", "192.0.2.1/24")
	addr("isp1", "lo", "203.0.113.10/32")
	addr("isp1", "lo", "203.0.113.50/32")
	addr("", "eth2", "198.51.100.2/24")
	addr("isp2", "wan0", "198.51.100.1/24")
	addr("isp2", "lo", "203.0.113.10/32")
	NS(t, "isp1", "ip", "route", "add", "default", "via", "192.0.2.2")
	Sh(t, "ip", "route", "add", "default", "via", "192.0.2.1", "dev", "eth1")
	// strict reverse-path filtering on the WAN devices: swanix must loosen it
	for _, kv := range []string{"net.ipv4.ip_forward=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0",
		"net.ipv4.conf.eth1.rp_filter=1", "net.ipv4.conf.eth2.rp_filter=1", "net.ipv4.conf.br-lan.rp_filter=0"} {
		Sh(t, "sysctl", "-qw", kv)
	}
	// minimal stand-in for fw4: masquerade on the WANs and a port forward
	fw4 := `
table inet fw4sim {
	chain srcnat {
		type nat hook postrouting priority srcnat; policy accept;
		oifname { "eth1", "eth2" } masquerade
	}
	chain dstnat {
		type nat hook prerouting priority dstnat; policy accept;
		iifname { "eth1", "eth2" } tcp dport 8080 dnat ip to 192.168.1.100:8080
		iifname { "eth1", "eth2" } udp dport 9999 dnat ip to 192.168.1.100:9999
	}
}
`
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(fw4)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fw4 stand-in: %v\n%s", err, out)
	}
	tp.StartServer("isp1", "wan1", "203.0.113.10", 80)
	tp.StartServer("isp2", "wan2", "203.0.113.10", 80)
	tp.StartServer("lan", "lan-server", "192.168.1.100", 8080)
	return tp
}

// UDPProbe sends one datagram from a namespace (bound to src) to dst:port and
// reports "refused" when an ICMP port-unreachable comes back, "timeout" otherwise.
func UDPProbe(t *testing.T, ns, src, dst string, port int) string {
	t.Helper()
	return strings.TrimSpace(NS(t, ns, "python3", "-I", "-c", `
import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(2); s.bind((sys.argv[1],0)); s.connect((sys.argv[2],int(sys.argv[3])))
r="timeout"
for i in range(3):
    try:
        s.send(b"x"); s.recv(64); r="answered"; break
    except ConnectionRefusedError:
        r="refused"; break
    except socket.timeout:
        pass
print(r)`, src, dst, fmt.Sprint(port)))
}

// inNS builds a command for a named namespace, or for the router itself when ns is empty.
func inNS(ns string, args ...string) *exec.Cmd {
	if ns == "" {
		return exec.Command(args[0], args[1:]...)
	}
	return exec.Command("ip", append([]string{"netns", "exec", ns}, args...)...)
}

// AddDestination adds another "internet" address served by both ISPs, each
// answering with its own name.
func (tp *Topo) AddDestination(addr string) {
	for i, ns := range []string{"isp1", "isp2"} {
		NS(tp.T, ns, "ip", "addr", "add", addr+"/32", "dev", "lo")
		tp.StartServer(ns, fmt.Sprintf("wan%d", i+1), addr, 80)
	}
}

// StartServer starts (or restarts) a banner server in a namespace.
func (tp *Topo) StartServer(ns, banner, host string, port int) {
	t := tp.T
	key := ns + "/" + host
	tp.StopServer(ns, host)
	cmd := inNS(ns, "python3", "-I", "-c", serverPy, banner, host, fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tp.servers[key] = cmd
	for i := 0; i < 50; i++ {
		out, _ := inNS(ns, "python3", "-I", "-c",
			"import socket,sys;s=socket.socket();s.settimeout(.2);\ntry: s.connect((sys.argv[1],int(sys.argv[2])));print('up')\nexcept Exception: pass", host, fmt.Sprint(port)).Output()
		if strings.Contains(string(out), "up") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server %s did not start", banner)
}

// StopServer kills a server started with StartServer.
func (tp *Topo) StopServer(ns, host string) {
	key := ns + "/" + host
	if c, ok := tp.servers[key]; ok {
		_ = c.Process.Signal(syscall.SIGKILL)
		_ = c.Wait()
		delete(tp.servers, key)
	}
}

// Count makes n sequential connections to 203.0.113.10:80 from ns ("" is the
// router itself) and tallies the answers. extra is [mark [bind]].
func (tp *Topo) Count(ns string, n int, extra ...string) map[string]int {
	tp.T.Helper()
	return tp.CountTo(ns, "203.0.113.10", n, extra...)
}

// CountTo is Count against another "internet" address (see AddDestination).
func (tp *Topo) CountTo(ns, dst string, n int, extra ...string) map[string]int {
	t := tp.T
	t.Helper()
	args := []string{"python3", "-I", "-c", clientPy, dst, "80", fmt.Sprint(n)}
	args = append(args, extra...)
	var out string
	if ns == "" {
		out = Sh(t, args[0], args[1:]...)
	} else {
		out = NS(t, ns, args...)
	}
	res := map[string]int{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("bad client output %q: %v", out, err)
	}
	return res
}

// Persistent opens one connection from the LAN, returns its first banner and a
// function that sends one more byte and returns the next banner.
func (tp *Topo) Persistent() (first string, again func() string) {
	t := tp.T
	cmd := exec.Command("ip", "netns", "exec", "lan", "python3", "-I", "-c", `
import socket,sys
s=socket.create_connection(("203.0.113.10",80),timeout=3); print(s.recv(32).decode().strip(),flush=True)
sys.stdin.readline(); s.sendall(b"p"); print(s.recv(32).decode().strip(),flush=True)`)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(stdout)
	line, _ := rd.ReadString('\n')
	return strings.TrimSpace(line), func() string {
		fmt.Fprintln(stdin, "go")
		l, _ := rd.ReadString('\n')
		_ = cmd.Wait()
		return strings.TrimSpace(l)
	}
}

// FakeUbus installs a `ubus` stand-in on PATH that serves
// network.interface.<name> status from dir/<name>.json.
func FakeUbus(t *testing.T, dir string, ifaces map[string]string) {
	t.Helper()
	for name, js := range ifaces {
		if err := os.WriteFile(dir+"/"+name+".json", []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
if [ "$1" = list ]; then exit 0; fi
if [ "$1" = call ]; then
	n="${2#network.interface.}"
	if [ -f "` + dir + `/$n.json" ]; then cat "` + dir + `/$n.json"; exit 0; fi
	echo "Command failed: Not found" >&2; exit 4
fi
exit 1
`
	bin := dir + "/bin"
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+"/ubus", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// Within asserts got/total is near want.
func Within(t *testing.T, name string, got, total int, want, tol float64) {
	t.Helper()
	f := float64(got) / float64(total)
	if f < want-tol || f > want+tol {
		t.Errorf("%s: %d/%d = %.2f, want %.2f ± %.2f", name, got, total, f, want, tol)
	}
}

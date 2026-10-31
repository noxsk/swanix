//go:build !mw_lite

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naive.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"naive.test"}, IPAddresses: []net.IP{net.ParseIP("203.0.113.10")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

// TestFullNaiveWANPolicy sends real naive (cronet) traffic through the release-grade Full
// binary. SWANIX_NAIVE_BIN must be the cronet build (CI artifact); a plain sing-box built from
// this tree (SWANIX_NAIVE_SERVER) plays the naive server inside each ISP namespace. It shows that cronet's outer
// connection is dialled through wan_policy and that a new session fails over to another line.
func TestFullNaiveWANPolicy(t *testing.T) {
	nstest.Require(t)
	bin := os.Getenv("SWANIX_NAIVE_BIN")
	if bin == "" {
		t.Skip("set SWANIX_NAIVE_BIN to the cronet-linked Full binary")
	}
	tp := nstest.New(t)
	dir := t.TempDir()
	cert, key := writeSelfSigned(t, dir)

	server := os.Getenv("SWANIX_NAIVE_SERVER")
	if server == "" {
		t.Skip("set SWANIX_NAIVE_SERVER to a sing-box binary built from this tree (go build ./cmd/sing-box)")
	}
	serverCfg := fmt.Sprintf(`{"log":{"level":"warn"},
 "inbounds":[{"type":"naive","tag":"n","listen":"203.0.113.10","listen_port":8443,"network":"tcp",
   "users":[{"username":"u","password":"p"}],"tls":{"enabled":true,"certificate_path":%q,"key_path":%q}}],
 "outbounds":[{"type":"direct","tag":"d"}],"route":{"final":"d"}}`, cert, key)
	serverPath := filepath.Join(dir, "server.json")
	if err := os.WriteFile(serverPath, []byte(serverCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"isp1", "isp2"} {
		cmd := exec.Command("ip", "netns", "exec", ns, server, "run", "-c", serverPath)
		var log strings.Builder
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			if t.Failed() {
				t.Logf("naive server in %s:\n%s", ns, log.String())
			}
		})
	}
	time.Sleep(2 * time.Second)

	proxy := fmt.Sprintf(`{"log":{"level":"warn"},
 "inbounds":[{"type":"socks","tag":"in","listen":"192.168.1.1","listen_port":1080}],
 "outbounds":[{"type":"naive","tag":"nv","server":"203.0.113.10","server_port":8443,"username":"u","password":"p",
   "tls":{"enabled":true,"server_name":"naive.test","certificate_path":%q},"wan_policy":"balanced"}],
 "route":{"final":"nv"}}`, cert)
	proxyPath := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(proxyPath, []byte(proxy), 0o600); err != nil {
		t.Fatal(err)
	}
	// wan1 is a backup: while wan2 is usable every new outer connection must use wan2
	cfg := strings.Replace(e2eConfig, "config wan 'wan1'\n\toption id '1'", "config wan 'wan1'\n\toption id '1'\n\toption role 'backup'", 1)
	cfg = strings.Replace(cfg, "config ipv6 'ipv6'", "config proxy 'proxy'\n\toption enabled '1'\n\toption config_file '"+proxyPath+"'\n\nconfig ipv6 'ipv6'", 1)
	cfgPath := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	t.Setenv("SWANIX_CONFIG", cfgPath)

	if out, err := exec.Command(bin, "check").CombinedOutput(); err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	daemon := exec.Command(bin, "run")
	var dlog strings.Builder
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGKILL)
		_ = daemon.Wait()
		if t.Failed() {
			t.Logf("daemon log:\n%s", dlog.String())
		}
	})
	waitFor(t, "both lines healthy", 40*time.Second, func() bool {
		st, ok := status(t, bin)
		if !ok {
			return false
		}
		a, _ := state(st, "wan1")
		b, sb := state(st, "wan2")
		return a == "healthy" && b == "healthy" && sb == 100
	})

	count := func(n string) map[string]int {
		return parseCounts(t, nstest.NS(t, "lan", "python3", "-I", "-c", socksClientPy, n))
	}
	t.Run("naive sessions use the line the policy picks", func(t *testing.T) {
		r := count("30")
		if r["wan2"] != 30 {
			t.Fatalf("expected every connection through wan2 (wan1 is backup): %v", r)
		}
	})
	t.Run("a new session fails over when that line dies", func(t *testing.T) {
		tp.StopServer("isp2", "203.0.113.10")
		_ = exec.Command("ip", "netns", "exec", "isp2", "ip", "link", "set", "wan0", "down").Run()
		waitFor(t, "wan2 down", 30*time.Second, func() bool {
			st, _ := status(t, bin)
			s, _ := state(st, "wan2")
			return s == "down"
		})
		var r map[string]int
		waitFor(t, "traffic through wan1", 60*time.Second, func() bool {
			r = count("5")
			return r["wan1"] == 5
		})
	})
}

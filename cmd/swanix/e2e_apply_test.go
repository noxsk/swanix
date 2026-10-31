package main

import (
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

// fake procd-style init scripts keep their state in plain files so the test can
// see exactly what `swanix apply/confirm/rollback` did to services.
const fakeInit = `#!/bin/sh
name=$(basename "$0")
st="$SWANIX_FAKE_STATE/$name"
mkdir -p "$st"
case "$1" in
start|restart)
	[ -f "$st/pid" ] && kill "$(cat "$st/pid")" 2>/dev/null
	if [ "$name" = swanix ]; then
		setsid "$SWANIX_BIN" run >"$st/log" 2>&1 &
		echo $! > "$st/pid"
	fi
	touch "$st/running" ;;
stop)
	[ -f "$st/pid" ] && kill "$(cat "$st/pid")" 2>/dev/null
	rm -f "$st/pid" "$st/running" ;;
running) [ -f "$st/running" ] ;;
enabled) [ -f "$st/enabled" ] ;;
enable) touch "$st/enabled" ;;
disable) rm -f "$st/enabled" ;;
esac
exit 0
`

// stopDaemon kills the daemon started by the fake init script, by its recorded PID only.
func stopDaemon(state string) {
	if b, err := os.ReadFile(filepath.Join(state, "swanix", "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func has(dir, svc, flag string) bool {
	_, err := os.Stat(filepath.Join(dir, svc, flag))
	return err == nil
}

// TestApplyConfirmRollback drives the real CLI through the confirmation window:
// swanix replaces a running "mwan3", the operator confirms or does not, and the
// out-of-process guard restores everything on timeout.
func TestApplyConfirmRollback(t *testing.T) {
	nstest.Require(t)
	tp := nstest.New(t)
	_ = tp
	dir := t.TempDir()

	// shadow /etc/init.d with fake scripts (private mount namespace: the host is untouched)
	initDir := filepath.Join(dir, "init.d")
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"swanix", "mwan3"} {
		if err := os.WriteFile(filepath.Join(initDir, n), []byte(fakeInit), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("mount", "--bind", initDir, "/etc/init.d").CombinedOutput(); err != nil {
		t.Skipf("cannot shadow /etc/init.d (%v: %s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", "/etc/init.d").Run() })

	state := filepath.Join(dir, "state")
	bin := filepath.Join(dir, "swanix")
	build := exec.Command("go", "build", "-tags", "mw_lite", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	cfgPath := filepath.Join(dir, "swanix.conf")
	original := strings.Replace(strings.Replace(e2eConfig, "option enabled '1'", "option enabled '0'", 1), "option startup_mode 'manage'", "option startup_mode 'observe'", 1)
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWANIX_CONFIG", cfgPath)
	t.Setenv("SWANIX_FAKE_STATE", state)
	t.Setenv("SWANIX_BIN", bin)
	t.Cleanup(func() {
		_ = exec.Command(bin, "rollback").Run()
		stopDaemon(state)
	})

	// the old controller is running and enabled
	for _, f := range []string{"running", "enabled"} {
		if err := os.MkdirAll(filepath.Join(state, "mwan3"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "mwan3", f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the operator edits the configuration (prepare saves the pre-edit copy first)
	prepare := func() {
		if err := os.MkdirAll("/var/run/swanix", 0o750); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(cfgPath)
		if err := os.WriteFile("/var/run/swanix/config.pre", b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, []byte(e2eConfig), 0o644); err != nil { // enabled, manage
			t.Fatal(err)
		}
	}

	run := func(args ...string) (string, error) {
		out, err := exec.Command(bin, args...).CombinedOutput()
		return string(out), err
	}

	t.Run("apply then confirm keeps the new setup", func(t *testing.T) {
		prepare()
		out, err := run("apply", "--timeout", "30s")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		if has(state, "mwan3", "running") {
			t.Fatal("mwan3 must be stopped before swanix takes over")
		}
		if !has(state, "swanix", "running") {
			t.Fatal("swanix was not started")
		}
		if o := nstest.Sh(t, "nft", "list", "tables"); !strings.Contains(o, " swanix") {
			t.Fatal("nft table missing after apply")
		}
		if o, err := run("apply"); err == nil {
			t.Fatalf("a second apply during the window must be refused: %s", o)
		}
		if out, err := run("confirm"); err != nil {
			t.Fatalf("confirm: %v\n%s", err, out)
		}
		if !has(state, "swanix", "enabled") || has(state, "mwan3", "enabled") {
			t.Fatal("boot enablement not switched on confirm")
		}
		if _, err := os.Stat("/var/run/swanix/apply.json"); err == nil {
			t.Fatal("pending state not cleared")
		}
		// after the window the guard must not undo a confirmed switch
		time.Sleep(2 * time.Second)
		if o := nstest.Sh(t, "nft", "list", "tables"); !strings.Contains(o, " swanix") {
			t.Fatal("guard rolled back a confirmed switch")
		}
	})

	t.Run("no confirmation: the guard restores everything", func(t *testing.T) {
		// back to the starting point
		if out, err := run("rollback"); err != nil && !strings.Contains(out, "") {
			t.Logf("cleanup rollback: %v", err)
		}
		stopDaemon(state)
		_ = os.WriteFile(cfgPath, []byte(original), 0o644)
		for _, f := range []string{"running", "enabled"} {
			_ = os.WriteFile(filepath.Join(state, "mwan3", f), nil, 0o644)
		}
		_ = os.Remove(filepath.Join(state, "swanix", "enabled"))
		_ = exec.Command(bin, "remove").Run()

		prepare()
		if out, err := run("apply", "--timeout", "10s"); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		if !has(state, "swanix", "running") || has(state, "mwan3", "running") {
			t.Fatal("takeover did not happen")
		}
		// never confirm; the guard (a separate process) must roll back at the deadline
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat("/var/run/swanix/apply.json"); os.IsNotExist(err) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if _, err := os.Stat("/var/run/swanix/apply.json"); err == nil {
			lg, _ := os.ReadFile("/var/run/swanix/guard.log")
			js, _ := os.ReadFile("/var/run/swanix/apply.json")
			ps, _ := exec.Command("ps", "-eo", "pid,stat,args").CombinedOutput()
			t.Fatalf("guard did not finish within the deadline; guard log:\n%s\nstate: %s\nps:\n%s", lg, js, ps)
		}
		if o := nstest.Sh(t, "nft", "list", "tables"); strings.Contains(o, " swanix") {
			t.Fatal("kernel objects left behind after rollback")
		}
		if !has(state, "mwan3", "running") {
			t.Fatal("mwan3 was not started again")
		}
		if has(state, "swanix", "running") || has(state, "swanix", "enabled") {
			t.Fatal("swanix still running/enabled after rollback")
		}
		if b, _ := os.ReadFile(cfgPath); string(b) != original {
			t.Fatal("configuration not restored")
		}
		if out, err := run("confirm"); err == nil {
			t.Fatalf("confirm after rollback must fail: %s", out)
		}
	})
}

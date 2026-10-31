package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls   []string
	running map[string]bool
	enabled map[string]bool
	failOn  string
}

func (f *fakeRunner) Run(_ string, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.failOn != "" && call == f.failOn {
		return "", errors.New("boom")
	}
	svc := filepath.Base(name)
	switch args[0] {
	case "running":
		if f.running[svc] {
			return "", nil
		}
		return "", errors.New("not running")
	case "enabled":
		if f.enabled[svc] {
			return "", nil
		}
		return "", errors.New("disabled")
	case "start", "restart":
		f.running[svc] = true
	case "stop":
		f.running[svc] = false
	case "enable":
		f.enabled[svc] = true
	case "disable":
		f.enabled[svc] = false
	}
	return "", nil
}

type clock struct{ t time.Time }

func rig(t *testing.T) (*Session, *fakeRunner, *clock, string, *int) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfg, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{running: map[string]bool{"mwan3": true}, enabled: map[string]bool{"mwan3": true}}
	clk := &clock{t: time.Unix(1000, 0)}
	removed := 0
	h := Hooks{
		Verify:       func() error { return nil },
		RemoveKernel: func() error { removed++; return nil },
		SpawnGuard:   func() (int, error) { return 4242, nil },
		KillGuard:    func(int) {},
	}
	s := New(fr, filepath.Join(dir, "run"), cfg, h)
	s.Exists = func(p string) bool {
		return p == "/etc/init.d/mwan3" || p == "/etc/init.d/swanix" || func() bool { _, err := os.Stat(p); return err == nil }()
	}
	s.Now = func() time.Time { return clk.t }
	s.Sleep = func(d time.Duration) { clk.t = clk.t.Add(d) }
	return s, fr, clk, cfg, &removed
}

func TestApplyThenConfirm(t *testing.T) {
	s, fr, _, cfg, _ := rig(t)
	if err := os.WriteFile(cfg, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	// old controller retired before the new one started
	stop, start := indexOf(fr.calls, "/etc/init.d/mwan3 stop"), indexOf(fr.calls, "/etc/init.d/swanix restart")
	if stop < 0 || start < 0 || stop > start {
		t.Fatalf("order wrong: %v", fr.calls)
	}
	if p, _ := s.Pending(); p == nil {
		t.Fatal("apply must leave a pending state")
	}
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	if !fr.enabled["swanix"] || fr.enabled["mwan3"] {
		t.Fatalf("enable state after confirm: %+v", fr.enabled)
	}
	if p, _ := s.Pending(); p != nil {
		t.Fatal("state must be cleared after confirm")
	}
}

func TestTimeoutRollsBackEverything(t *testing.T) {
	s, fr, _, cfg, removed := rig(t)
	// the operator staged a change: pre-edit copy is kept in config.pre
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, preName), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.Guard()
	if err != nil || confirmed {
		t.Fatalf("guard should roll back on timeout: confirmed=%v err=%v", confirmed, err)
	}
	if *removed != 1 {
		t.Fatal("kernel objects not removed")
	}
	if b, _ := os.ReadFile(cfg); string(b) != "old" {
		t.Fatalf("config not restored: %q", b)
	}
	if !fr.running["mwan3"] || fr.running["swanix"] {
		t.Fatalf("services not restored: %+v", fr.running)
	}
	if p, _ := s.Pending(); p != nil {
		t.Fatal("state must be cleared")
	}
	if err := s.Confirm(); err == nil {
		t.Fatal("confirming after rollback must fail")
	}
}

func TestVerifyFailureRollsBackImmediately(t *testing.T) {
	s, fr, _, _, removed := rig(t)
	s.Hooks.Verify = func() error { return errors.New("no WAN reachable") }
	if _, err := s.Apply(90 * time.Second); err == nil || !strings.Contains(err.Error(), "no WAN reachable") {
		t.Fatalf("got %v", err)
	}
	if *removed != 1 || !fr.running["mwan3"] {
		t.Fatalf("not rolled back: removed=%d running=%v", *removed, fr.running)
	}
}

func TestGuardPassesWhenConfirmed(t *testing.T) {
	s, _, _, _, removed := rig(t)
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.Guard()
	if err != nil || !confirmed || *removed != 0 {
		t.Fatalf("confirmed=%v err=%v removed=%d", confirmed, err, *removed)
	}
}

func TestSecondApplyRefused(t *testing.T) {
	s, _, _, _, _ := rig(t)
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(90 * time.Second); err == nil {
		t.Fatal("overlapping applies must be refused")
	}
}

func TestGuardDoesNotKillItselfWhenRollingBack(t *testing.T) {
	s, _, _, _, removed := rig(t)
	killed := 0
	// the "guard" is this very process, as it is in production when the guard rolls back
	s.Hooks.SpawnGuard = func() (int, error) { return os.Getpid(), nil }
	s.Hooks.KillGuard = func(int) { killed++ }
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := s.Guard(); err != nil || confirmed {
		t.Fatalf("expected a timeout rollback, got confirmed=%v err=%v", confirmed, err)
	}
	if killed != 0 {
		t.Fatal("the guard signalled itself during its own rollback")
	}
	if *removed != 1 {
		t.Fatal("rollback did not run")
	}
}

func TestStateExistsBeforeGuardStarts(t *testing.T) {
	s, _, _, _, _ := rig(t)
	s.Hooks.SpawnGuard = func() (int, error) {
		// a guard that starts now must already see the pending switch, or it would exit at once
		if p, _ := s.Pending(); p == nil {
			t.Error("guard spawned before the pending state was written")
		}
		return 4242, nil
	}
	if _, err := s.Apply(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(); p == nil || p.GuardPID != 4242 {
		t.Fatalf("guard pid not recorded: %+v", p)
	}
}

func TestGuardFailureAbortsBeforeChangingAnything(t *testing.T) {
	s, fr, _, _, _ := rig(t)
	s.Hooks.SpawnGuard = func() (int, error) { return 0, errors.New("fork failed") }
	if _, err := s.Apply(90 * time.Second); err == nil {
		t.Fatal("must not proceed without a guard")
	}
	for _, c := range fr.calls {
		if strings.HasSuffix(c, " stop") || strings.HasSuffix(c, " restart") {
			t.Fatalf("network touched without a guard: %v", fr.calls)
		}
	}
}

func indexOf(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}

func TestWindowStartsAfterVerification(t *testing.T) {
	s, _, clk, _, removed := rig(t)
	var during time.Time
	s.Hooks.Verify = func() error {
		// a slow start: verification takes longer than the confirmation window
		clk.t = clk.t.Add(30 * time.Second)
		st, _ := s.Pending()
		during = st.Deadline
		return nil
	}
	start := clk.t
	st, err := s.Apply(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !during.After(start.Add(30 * time.Second)) {
		t.Fatalf("the guard's deadline %v would have rolled back during verification (started %v)", during, start)
	}
	if want := start.Add(45 * time.Second); !st.Deadline.Equal(want) {
		t.Fatalf("window should start after verification: deadline %v, want %v", st.Deadline, want)
	}
	if p, _ := s.Pending(); p == nil || !p.Deadline.Equal(st.Deadline) {
		t.Fatalf("the guard reads the state file, which must carry the new deadline: %+v", p)
	}
	if confirmed, err := s.Guard(); err != nil || confirmed || *removed != 1 {
		t.Fatalf("the window still expires: confirmed=%v err=%v removed=%d", confirmed, err, *removed)
	}
}

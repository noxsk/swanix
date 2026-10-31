// Package session implements the confirm-or-rollback configuration switch:
// save state, stop mwan3, install swanix, verify, then wait for confirmation.
// The guard that enforces the deadline runs as a separate process that exists
// only for the duration of one apply, so a crashed daemon cannot strand a
// half-applied network.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
)

const (
	initDir     = "/etc/init.d/"
	svcSwanix   = "swanix"
	svcMwan3    = "mwan3"
	stateName   = "apply.json"
	backupName  = "config.bak"
	preName     = "config.pre"
	confirmName = "confirmed"
)

// State is persisted so the guard and CLI can act independently of the daemon.
type State struct {
	StartedAt        time.Time `json:"started_at"`
	Deadline         time.Time `json:"deadline"`
	ConfigPath       string    `json:"config_path"`
	Mwan3Existed     bool      `json:"mwan3_existed"`
	Mwan3WasRunning  bool      `json:"mwan3_was_running"`
	Mwan3WasEnabled  bool      `json:"mwan3_was_enabled"`
	SwanixWasEnabled bool      `json:"swanix_was_enabled"`
	GuardPID         int       `json:"guard_pid"`
}

// VerifyBudget is the default time Hooks.Verify may take (Session.Verify overrides
// it). The guard's deadline covers it while verification runs; the operator's window
// starts once it has passed.
const VerifyBudget = 40 * time.Second

// Hooks are the operations the session needs from the rest of the system.
type Hooks struct {
	// Verify checks that LAN management and outbound connectivity work, within VerifyBudget.
	Verify func() error
	// RemoveKernel withdraws swanix's kernel objects.
	RemoveKernel func() error
	// SpawnGuard starts the detached deadline guard and returns its PID.
	SpawnGuard func() (int, error)
	// KillGuard stops a running guard.
	KillGuard func(pid int)
}

// Session performs the switch.
type Session struct {
	R          kernel.Runner
	Dir        string // /var/run/swanix
	ConfigPath string // /etc/config/swanix
	Exists     func(path string) bool
	Now        func() time.Time
	Sleep      func(time.Duration)
	Hooks      Hooks
	Verify     time.Duration // verification budget; VerifyBudget when zero
}

// VerifyFor is the verification budget in effect.
func (s *Session) VerifyFor() time.Duration {
	if s.Verify > 0 {
		return s.Verify
	}
	return VerifyBudget
}

func New(r kernel.Runner, dir, configPath string, h Hooks) *Session {
	return &Session{R: r, Dir: dir, ConfigPath: configPath, Hooks: h,
		Exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		Now:    time.Now, Sleep: time.Sleep}
}

func (s *Session) path(n string) string { return filepath.Join(s.Dir, n) }

func (s *Session) svc(name, action string) error {
	_, err := s.R.Run("", initDir+name, action)
	return err
}

func (s *Session) svcTest(name, action string) bool {
	_, err := s.R.Run("", initDir+name, action)
	return err == nil
}

// Pending reports an apply that has not been confirmed or rolled back.
func (s *Session) Pending() (*State, error) {
	b, err := os.ReadFile(s.path(stateName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Session) save(st *State) error {
	b, _ := json.Marshal(st)
	return os.WriteFile(s.path(stateName), b, 0o600)
}

// Apply switches the router to swanix and starts the confirmation window.
func (s *Session) Apply(timeout time.Duration) (*State, error) {
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return nil, err
	}
	if p, err := s.Pending(); err != nil {
		return nil, err
	} else if p != nil {
		return nil, fmt.Errorf("已有未确认的切换（截止 %s）：请先 confirm 或 rollback", p.Deadline.Format(time.RFC3339))
	}

	// 1. save configuration and service state
	cfgBytes, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	backup := s.path(backupName)
	if pre, err := os.ReadFile(s.path(preName)); err == nil {
		cfgBytes = pre // the configuration as it was before the operator's edit
	}
	if err := os.WriteFile(backup, cfgBytes, 0o600); err != nil {
		return nil, err
	}
	_ = os.Remove(s.path(confirmName))
	// until verification has passed, the guard also covers its budget: a hung apply is
	// still rolled back, and a slow start is not rolled back before it was verified
	st := &State{StartedAt: s.Now(), Deadline: s.Now().Add(s.VerifyFor() + timeout), ConfigPath: s.ConfigPath,
		Mwan3Existed:     s.Exists(initDir + svcMwan3),
		SwanixWasEnabled: s.svcTest(svcSwanix, "enabled")}
	if st.Mwan3Existed {
		st.Mwan3WasRunning = s.svcTest(svcMwan3, "running")
		st.Mwan3WasEnabled = s.svcTest(svcMwan3, "enabled")
	}

	// 2. record the pending switch, then arm the guard before touching anything.
	// The state must exist before the guard starts: a guard that finds no pending
	// switch assumes it was already confirmed or rolled back and exits.
	if err := s.save(st); err != nil {
		return nil, err
	}
	if s.Hooks.SpawnGuard != nil {
		pid, err := s.Hooks.SpawnGuard()
		if err != nil {
			_ = os.Remove(s.path(stateName))
			_ = os.Remove(s.path(backupName))
			return nil, fmt.Errorf("无法启动回滚守护进程，已放弃切换: %w", err)
		}
		st.GuardPID = pid
		if err := s.save(st); err != nil {
			s.Hooks.KillGuard(pid)
			return nil, err
		}
	}

	// 3. retire the old controller first, then start the new one
	if st.Mwan3Existed && st.Mwan3WasRunning {
		if err := s.svc(svcMwan3, "stop"); err != nil {
			_ = s.Rollback("停止 mwan3 失败: " + err.Error())
			return nil, err
		}
	}
	if err := s.svc(svcSwanix, "restart"); err != nil {
		_ = s.Rollback("启动 swanix 失败: " + err.Error())
		return nil, err
	}

	// 4. verify before asking the operator to confirm
	if s.Hooks.Verify != nil {
		if err := s.Hooks.Verify(); err != nil {
			_ = s.Rollback("验证失败: " + err.Error())
			return nil, fmt.Errorf("验证失败，已回滚: %w", err)
		}
	}
	// 5. the confirmation window starts now; the guard re-reads the deadline
	st.Deadline = s.Now().Add(timeout)
	if err := s.save(st); err != nil {
		_ = s.Rollback("无法记录确认时限: " + err.Error())
		return nil, err
	}
	return st, nil
}

// Confirm keeps the new setup.
func (s *Session) Confirm() error {
	st, err := s.Pending()
	if err != nil {
		return err
	}
	if st == nil {
		return errors.New("没有待确认的切换")
	}
	if s.Now().After(st.Deadline) {
		return errors.New("确认时限已过，切换正在或已经回滚")
	}
	if err := os.WriteFile(s.path(confirmName), nil, 0o600); err != nil {
		return err
	}
	if s.Hooks.KillGuard != nil && st.GuardPID > 0 {
		s.Hooks.KillGuard(st.GuardPID)
	}
	if err := s.svc(svcSwanix, "enable"); err != nil {
		return err
	}
	if st.Mwan3Existed && st.Mwan3WasEnabled {
		// swanix replaces mwan3: keep it from coming back at boot, but leave the package installed
		if err := s.svc(svcMwan3, "disable"); err != nil {
			return err
		}
	}
	_ = os.Remove(s.path(backupName))
	_ = os.Remove(s.path(preName))
	return os.Remove(s.path(stateName))
}

// Rollback withdraws only swanix's own resources and restores the saved
// configuration and the previous controller's state.
func (s *Session) Rollback(reason string) error {
	st, err := s.Pending()
	if err != nil {
		return err
	}
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	_ = s.svc(svcSwanix, "stop")
	if s.Hooks.RemoveKernel != nil {
		keep(s.Hooks.RemoveKernel())
	}
	if st != nil {
		if b, err := os.ReadFile(s.path(backupName)); err == nil {
			keep(os.WriteFile(st.ConfigPath, b, 0o644))
		}
		if !st.SwanixWasEnabled {
			_ = s.svc(svcSwanix, "disable")
		}
		if st.Mwan3Existed && st.Mwan3WasRunning {
			keep(s.svc(svcMwan3, "start"))
		}
	}
	_ = os.Remove(s.path(backupName))
	_ = os.Remove(s.path(preName))
	_ = os.Remove(s.path(stateName))
	// Stop the guard last, and never from inside the guard itself: a guard that rolls back
	// at its deadline would otherwise SIGTERM itself before it finished cleaning up.
	if st != nil && s.Hooks.KillGuard != nil && st.GuardPID > 0 && st.GuardPID != os.Getpid() {
		s.Hooks.KillGuard(st.GuardPID)
	}
	if reason != "" {
		fmt.Fprintln(os.Stderr, "swanix: 已回滚:", reason)
	}
	return errors.Join(errs...)
}

// Guard waits for confirmation or the deadline and rolls back on timeout.
// It returns true when the switch was confirmed.
func (s *Session) Guard() (confirmed bool, err error) {
	for {
		st, err := s.Pending()
		if err != nil {
			return false, err
		}
		if st == nil {
			return true, nil // confirmed or rolled back by someone else
		}
		if s.Exists(s.path(confirmName)) {
			return true, nil
		}
		if !s.Now().Before(st.Deadline) {
			return false, s.Rollback("确认时限已到")
		}
		s.Sleep(time.Second)
	}
}

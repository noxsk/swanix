package kernel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// rp_filter handling: strict reverse-path filtering drops legitimate replies
// that arrive on a different WAN than the main table would use. Only the
// managed WAN devices are raised from strict (1) to loose (2); the original
// value is remembered so removal restores it. Nothing is disabled globally.

func (a *Applier) procPath(dev string) string {
	root := a.ProcSys
	if root == "" {
		root = "/proc/sys"
	}
	return filepath.Join(root, "net/ipv4/conf", dev, "rp_filter")
}

func (a *Applier) stateFile() string {
	dir := a.StateDir
	if dir == "" {
		dir = "/var/run/swanix"
	}
	return filepath.Join(dir, "rp_filter.json")
}

func (a *Applier) loadSaved() map[string]string {
	saved := map[string]string{}
	if b, err := os.ReadFile(a.stateFile()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	return saved
}

func (a *Applier) storeSaved(saved map[string]string) error {
	if len(saved) == 0 {
		if err := os.Remove(a.stateFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.stateFile()), 0o750); err != nil {
		return err
	}
	b, _ := json.Marshal(saved)
	return os.WriteFile(a.stateFile(), b, 0o600)
}

// ensureLooseRPFilter raises strict rp_filter on managed WAN devices.
func (a *Applier) ensureLooseRPFilter(in *Input) error {
	saved := a.loadSaved()
	for _, w := range in.WANs {
		for _, dev := range []string{w.Device4} {
			if dev == "" {
				continue
			}
			b, err := os.ReadFile(a.procPath(dev))
			if err != nil {
				continue // device gone or not IPv4
			}
			if strings.TrimSpace(string(b)) != "1" {
				continue
			}
			if _, have := saved[dev]; !have {
				saved[dev] = "1"
			}
			if err := os.WriteFile(a.procPath(dev), []byte("2"), 0o644); err != nil {
				return err
			}
		}
	}
	return a.storeSaved(saved)
}

// restoreRPFilter puts back every value we changed.
func (a *Applier) restoreRPFilter() error {
	saved := a.loadSaved()
	var firstErr error
	for dev, v := range saved {
		if _, err := os.Stat(a.procPath(dev)); err != nil {
			continue
		}
		if err := os.WriteFile(a.procPath(dev), []byte(v), 0o644); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := a.storeSaved(nil); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

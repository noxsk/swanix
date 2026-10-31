// Package health holds the per-WAN, per-address-family state machine and the
// probing logic. It must not depend on any proxy package.
package health

import "time"

// State is the health state of one WAN for one address family.
type State int

const (
	Unknown State = iota
	Healthy
	Degraded
	Down
	Recovering
)

func (s State) String() string {
	switch s {
	case Healthy:
		return "healthy"
	case Degraded:
		return "degraded"
	case Down:
		return "down"
	case Recovering:
		return "recovering"
	}
	return "unknown"
}

// Usable reports whether new connections may be assigned to the WAN.
func (s State) Usable() bool { return s != Down }

// Params are the round thresholds of a health profile.
type Params struct {
	DownRounds  int           // consecutive failed rounds before Down
	UpRounds    int           // consecutive good rounds before Recovering
	RecoverRamp time.Duration // time spent in Recovering before Healthy
}

// Transition records a state change for the "recent switches" view.
type Transition struct {
	At     time.Time
	From   State
	To     State
	Reason string
}

// Machine is not safe for concurrent use; the engine owns it.
type Machine struct {
	p          Params
	state      State
	fail, ok   int
	since      time.Time
	reason     string
	recoveryAt time.Time
}

func NewMachine(p Params, now time.Time) *Machine {
	return &Machine{p: p, state: Unknown, since: now, reason: "no probe result yet"}
}

func (m *Machine) State() State     { return m.state }
func (m *Machine) Since() time.Time { return m.since }
func (m *Machine) Reason() string   { return m.reason }

// RecoveryElapsed is how long the WAN has been ramping back; zero otherwise.
func (m *Machine) RecoveryElapsed(now time.Time) time.Duration {
	if m.state != Recovering {
		return 0
	}
	return now.Sub(m.recoveryAt)
}

func (m *Machine) set(now time.Time, to State, reason string) *Transition {
	if m.state == to {
		m.reason = reason
		return nil
	}
	t := &Transition{At: now, From: m.state, To: to, Reason: reason}
	m.state, m.since, m.reason = to, now, reason
	m.fail, m.ok = 0, 0
	if to == Recovering {
		m.recoveryAt = now
	}
	return t
}

// Force sets the state without probe evidence. It is used by observe mode,
// where only netifd link state is available, and says so in the reason.
func (m *Machine) Force(now time.Time, to State, reason string) *Transition {
	return m.set(now, to, reason)
}

// LinkEvent handles netifd/rtnetlink events immediately. A link coming up does
// not make the WAN usable: the WAN stays Down until probes succeed.
func (m *Machine) LinkEvent(now time.Time, up bool, reason string) *Transition {
	if !up {
		return m.set(now, Down, reason)
	}
	if m.state == Down {
		m.reason = "link up, waiting for probes"
	}
	return nil
}

// Observe feeds one completed probe round. roundOK means the configured number
// of independent targets succeeded. degraded flags a usable but impaired line.
func (m *Machine) Observe(now time.Time, roundOK, degraded bool) *Transition {
	if roundOK {
		m.fail = 0
		m.ok++
	} else {
		m.ok = 0
		m.fail++
	}
	switch m.state {
	case Unknown:
		if roundOK {
			return m.set(now, Healthy, "probes succeeded")
		}
		if m.fail >= m.p.DownRounds {
			return m.set(now, Down, "probes failing")
		}
	case Healthy:
		if m.fail >= m.p.DownRounds {
			return m.set(now, Down, "probes failing")
		}
		if roundOK && degraded {
			return m.set(now, Degraded, "latency or loss above threshold")
		}
	case Degraded:
		if m.fail >= m.p.DownRounds {
			return m.set(now, Down, "probes failing")
		}
		if roundOK && !degraded {
			return m.set(now, Healthy, "quality restored")
		}
	case Down:
		if m.ok >= m.p.UpRounds {
			if m.p.RecoverRamp <= 0 {
				return m.set(now, Healthy, "probes recovered")
			}
			return m.set(now, Recovering, "probes recovered")
		}
		if !roundOK {
			m.reason = "probes failing"
		}
	case Recovering:
		if !roundOK {
			return m.set(now, Down, "probe failed during recovery")
		}
		if now.Sub(m.recoveryAt) >= m.p.RecoverRamp {
			return m.set(now, Healthy, "recovery complete")
		}
	}
	return nil
}

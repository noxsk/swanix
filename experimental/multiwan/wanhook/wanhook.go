// Package wanhook lets the Full build's dialers pick a WAN right before the
// socket connects. It has no dependency on the proxy packages.
package wanhook

import (
	"errors"
	"net/netip"
	"sync"
)

// Selection is one chosen WAN for one outgoing connection.
type Selection struct {
	WAN  int
	Mark uint32     // SO_MARK value (WAN ID bits only, no probe/inbound flags)
	Src  netip.Addr // local address to bind, may be invalid
}

// Selector picks a WAN for a policy and address family (4 or 6).
type Selector interface {
	Pick(policy string, family int) (Selection, error)
}

var (
	mu       sync.RWMutex
	selector Selector
)

// ErrNoSelector is returned while the MultiWAN service is not running; dials
// fail closed rather than leave through an arbitrary line.
var ErrNoSelector = errors.New("multiwan: service not running, cannot select WAN")

// Register installs the active selector; pass nil to remove it.
func Register(s Selector) {
	mu.Lock()
	selector = s
	mu.Unlock()
}

// Pick delegates to the registered selector.
func Pick(policy string, family int) (Selection, error) {
	mu.RLock()
	s := selector
	mu.RUnlock()
	if s == nil {
		return Selection{}, ErrNoSelector
	}
	return s.Pick(policy, family)
}

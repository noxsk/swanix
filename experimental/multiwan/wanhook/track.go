package wanhook

import (
	"io"
	"net"
	"runtime"
	"sync"
	"syscall"
	"weak"
)

// Sockets dialled through wan_policy are remembered by the WAN mark they carry,
// so they can be closed when that WAN goes down: a long-lived session (mux,
// HTTP/2, QUIC) otherwise keeps sending into a dead line until its own timeouts
// fire, and only then redials through a healthy one. The registry holds weak
// pointers, so it never keeps a socket alive; an entry disappears when its socket
// is garbage collected.

type trackKey struct {
	mark   uint32
	family int
}

type entry interface{ closer() io.Closer }

type weakTCP struct{ p weak.Pointer[net.TCPConn] }
type weakUDP struct{ p weak.Pointer[net.UDPConn] }

func (w weakTCP) closer() io.Closer {
	if c := w.p.Value(); c != nil {
		return c
	}
	return nil
}

func (w weakUDP) closer() io.Closer {
	if c := w.p.Value(); c != nil {
		return c
	}
	return nil
}

var (
	trackMu sync.Mutex
	tracked = map[trackKey]map[uint64]entry{}
	nextID  uint64
)

// Track registers a socket created through wan_policy. Only the standard
// library's TCP and UDP sockets are tracked; anything else is ignored. An
// unconnected UDP socket may send to either family, so it counts for both.
func Track(c any) {
	switch s := c.(type) {
	case *net.TCPConn:
		trackSocket(s, s, weakTCP{weak.Make(s)}, familiesOf(s.LocalAddr(), true))
	case *net.UDPConn:
		trackSocket(s, s, weakUDP{weak.Make(s)}, familiesOf(s.LocalAddr(), s.RemoteAddr() != nil))
	}
}

func familiesOf(local net.Addr, connected bool) []int {
	if !connected {
		return []int{4, 6}
	}
	var ip net.IP
	switch a := local.(type) {
	case *net.TCPAddr:
		ip = a.IP
	case *net.UDPAddr:
		ip = a.IP
	}
	if ip.To4() == nil {
		return []int{6}
	}
	return []int{4}
}

type registration struct {
	keys []trackKey
	id   uint64
}

func trackSocket[T any](s *T, sc syscall.Conn, e entry, families []int) {
	mark, ok := socketMark(sc)
	if !ok || mark == 0 {
		return
	}
	trackMu.Lock()
	nextID++
	r := registration{id: nextID}
	for _, f := range families {
		k := trackKey{mark, f}
		if tracked[k] == nil {
			tracked[k] = map[uint64]entry{}
		}
		tracked[k][r.id] = e
		r.keys = append(r.keys, k)
	}
	trackMu.Unlock()
	runtime.AddCleanup(s, unregister, r)
}

func unregister(r registration) {
	trackMu.Lock()
	defer trackMu.Unlock()
	for _, k := range r.keys {
		delete(tracked[k], r.id)
		if len(tracked[k]) == 0 {
			delete(tracked, k)
		}
	}
}

// Interrupt closes every open socket that was dialled through the WAN with this
// mark for the address family, and returns how many it closed.
func Interrupt(mark uint32, family int) int {
	trackMu.Lock()
	var cs []io.Closer
	for _, e := range tracked[trackKey{mark, family}] {
		if c := e.closer(); c != nil {
			cs = append(cs, c)
		}
	}
	trackMu.Unlock()
	closed := 0
	for _, c := range cs {
		if c.Close() == nil { // sockets the proxy already closed are only waiting for collection
			closed++
		}
	}
	return closed
}

// Tracked reports how many sockets are registered for a WAN mark and family
// (including closed ones not yet collected); for tests and status.
func Tracked(mark uint32, family int) int {
	trackMu.Lock()
	defer trackMu.Unlock()
	return len(tracked[trackKey{mark, family}])
}

//go:build linux

package wanhook

import (
	"fmt"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// ControlFunc is installed as net.Dialer.Control. It runs for every outgoing
// socket just before connect: it selects the WAN, sets SO_MARK and binds the
// WAN's source address so the first packet already has the right identity.
func ControlFunc(policy string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		family := 4
		if len(network) > 0 && network[len(network)-1] == '6' {
			family = 6
		}
		if ap, err := netip.ParseAddrPort(address); err == nil && ap.Addr().Is6() && !ap.Addr().Is4In6() {
			family = 6
		}
		sel, err := Pick(policy, family)
		if err != nil {
			return err
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(sel.Mark)); serr != nil {
				return
			}
			if sel.Src.IsValid() && sel.Src.Is4() == (family == 4) {
				var sa unix.Sockaddr
				if family == 4 {
					sa = &unix.SockaddrInet4{Addr: sel.Src.As4()}
				} else {
					sa = &unix.SockaddrInet6{Addr: sel.Src.As16()}
				}
				serr = unix.Bind(int(fd), sa)
			}
		}); err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("multiwan: prepare socket for WAN %d: %w", sel.WAN, serr)
		}
		return nil
	}
}

// socketMark reads SO_MARK, i.e. the WAN a wan_policy socket was given.
func socketMark(sc syscall.Conn) (uint32, bool) {
	rc, err := sc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var mark int
	var gerr error
	if err := rc.Control(func(fd uintptr) { mark, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK) }); err != nil || gerr != nil {
		return 0, false
	}
	return uint32(mark), true
}

// ListenControlFunc is the wan_policy hook for unconnected UDP sockets (ListenPacket).
// Such a socket may later send to either family, so it only gets the WAN's mark and
// no source address: the WAN's table and the firewall's masquerading pick the source.
func ListenControlFunc(policy string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		family := 4
		if network == "udp6" {
			family = 6
		} else if ap, err := netip.ParseAddrPort(address); err == nil && ap.Addr().Is6() && !ap.Addr().Is4In6() && !ap.Addr().IsUnspecified() {
			family = 6
		}
		sel, err := Pick(policy, family)
		if err != nil {
			return err
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(sel.Mark))
		}); err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("multiwan: mark socket for WAN %d: %w", sel.WAN, serr)
		}
		return nil
	}
}

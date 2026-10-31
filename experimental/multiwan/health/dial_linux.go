//go:build linux

package health

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// MarkedDial dials with SO_MARK set before connect, so the very first packet
// is routed by the WAN's probe table, and bound to the WAN's device when known.
func MarkedDial(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
	d := net.Dialer{
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark)); serr == nil && dev != "" {
					serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, dev)
				}
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return d.DialContext(ctx, network, addr)
}

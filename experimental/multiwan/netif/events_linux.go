//go:build linux

package netif

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// Watch blocks until ctx is done, calling notify (debounced) whenever the
// kernel reports a link, address or route change. Missed events are covered by
// the engine's periodic reconcile.
func Watch(ctx context.Context, debounce time.Duration, notify func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	groups := uint32(unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV6_ROUTE)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	buf := make([]byte, 1<<16)
	var pending bool
	var last time.Time
	for ctx.Err() == nil {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		switch {
		case err == nil && n > 0:
			pending, last = true, time.Now()
		case err == unix.EAGAIN || err == unix.EINTR || err == unix.EWOULDBLOCK:
		case err == unix.ENOBUFS:
			pending, last = true, time.Now() // overflow: assume everything changed
		default:
			return fmt.Errorf("netlink recv: %w", err)
		}
		if pending && time.Since(last) >= debounce {
			pending = false
			notify()
		}
	}
	return ctx.Err()
}

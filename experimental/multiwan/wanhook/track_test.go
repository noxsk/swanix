//go:build linux

package wanhook

import (
	"context"
	"errors"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testMark = 0x0a000000

func markControl(t *testing.T) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, testMark) }); err != nil {
			return err
		}
		if errors.Is(serr, unix.EPERM) {
			t.Skip("SO_MARK needs CAP_NET_ADMIN")
		}
		return serr
	}
}

func TestInterruptClosesTrackedSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	d := net.Dialer{Control: markControl(t)}
	c, err := d.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	Track(c)
	lc := net.ListenConfig{Control: markControl(t)}
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	Track(pc)
	if n4, n6 := Tracked(testMark, 4), Tracked(testMark, 6); n4 != 2 || n6 != 1 {
		t.Fatalf("tracked v4=%d v6=%d, want 2 and 1 (an unconnected UDP socket counts for both families)", n4, n6)
	}
	if n := Interrupt(testMark+1, 4); n != 0 {
		t.Fatalf("another WAN's mark closed %d sockets", n)
	}
	if n := Interrupt(testMark, 4); n != 2 {
		t.Fatalf("closed %d, want 2", n)
	}
	if n := Interrupt(testMark, 4); n != 0 {
		t.Fatalf("a second interrupt reported %d already closed sockets", n)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the TCP socket is still usable after the interrupt: %v", err)
	}
	if _, err := pc.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the UDP socket is still usable after the interrupt: %v", err)
	}
	// the registry holds weak pointers: dropped sockets leave it
	c, pc = nil, nil
	deadline := time.Now().Add(5 * time.Second)
	for Tracked(testMark, 4)+Tracked(testMark, 6) > 0 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if n := Tracked(testMark, 4) + Tracked(testMark, 6); n != 0 {
		t.Fatalf("%d entries left after the sockets were collected", n)
	}
}

func TestUnmarkedSocketsAreNotTracked(t *testing.T) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	Track(c)
	if Tracked(0, 4) != 0 {
		t.Fatal("a socket without a WAN mark was tracked")
	}
}

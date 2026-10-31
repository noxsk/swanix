//go:build !linux

package wanhook

import (
	"errors"
	"syscall"
)

func ControlFunc(policy string) func(network, address string, c syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error { return errors.New("wan_policy requires Linux") }
}

func ListenControlFunc(policy string) func(network, address string, c syscall.RawConn) error {
	return ControlFunc(policy)
}

func socketMark(syscall.Conn) (uint32, bool) { return 0, false }

//go:build !linux

package health

import (
	"context"
	"errors"
	"net"
)

func MarkedDial(ctx context.Context, network, addr string, mark uint32, dev string) (net.Conn, error) {
	return nil, errors.New("marked probes require Linux")
}

//go:build !linux

package netif

import (
	"context"
	"errors"
	"time"
)

func Watch(ctx context.Context, debounce time.Duration, notify func()) error {
	return errors.New("netlink events require Linux")
}

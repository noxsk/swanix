// Package control is the local control interface: a root-only Unix socket
// carrying one JSON request and one JSON reply per connection. There is no
// network listener and no shell execution.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Methods the daemon answers.
const (
	MethodStatus  = "status"
	MethodHistory = "history"
	MethodFeature = "features"
	MethodPoke    = "reconcile"
)

type request struct {
	Method string `json:"method"`
}

type reply struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Handler answers control requests.
type Handler func(method string) (any, error)

// Serve listens on path (mode 0600) until ctx is done.
func Serve(ctx context.Context, path string, h Handler) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	_ = os.Remove(path)
	old := setUmask(0o177)
	ln, err := net.Listen("unix", path)
	setUmask(old)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveConn(c, h)
	}
}

func serveConn(c net.Conn, h Handler) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var req request
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		_ = json.NewEncoder(c).Encode(reply{Error: "bad request"})
		return
	}
	res, err := h(req.Method)
	if err != nil {
		_ = json.NewEncoder(c).Encode(reply{Error: err.Error()})
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		_ = json.NewEncoder(c).Encode(reply{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(c).Encode(reply{OK: true, Result: b})
}

// Call sends one request and returns the raw result.
func Call(path, method string) (json.RawMessage, error) {
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("swanix 守护进程未运行或无法连接 (%s): %w", path, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(request{Method: method}); err != nil {
		return nil, err
	}
	var r reply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return r.Result, nil
}

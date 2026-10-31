package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTripAndPermissions(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "run", "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, sock, func(m string) (any, error) {
		if m == "boom" {
			return nil, errors.New("nope")
		}
		return map[string]string{"method": m}, nil
	})
	var raw json.RawMessage
	var err error
	for i := 0; i < 50; i++ {
		if raw, err = Call(sock, "status"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || string(raw) != `{"method":"status"}` {
		t.Fatalf("%s %v", raw, err)
	}
	if _, err := Call(sock, "boom"); err == nil || err.Error() != "nope" {
		t.Fatalf("got %v", err)
	}
	st, _ := os.Stat(sock)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", st.Mode().Perm())
	}
}

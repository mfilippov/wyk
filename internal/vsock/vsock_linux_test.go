//go:build linux

package vsock

import (
	"errors"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestSockaddrVMSize(t *testing.T) {
	if n := unsafe.Sizeof(sockaddrVM{}); n != 16 {
		t.Fatalf("sockaddr_vm is 16 bytes, got %d", n)
	}
}

// Nothing listens on this port, so Dial must fail, and within the timeout.
func TestDialNoListener(t *testing.T) {
	start := time.Now()
	c, err := Dial(CIDHost, 0x7ffffff0, time.Second)
	if errors.Is(err, syscall.EAFNOSUPPORT) {
		t.Skip("no AF_VSOCK on this machine")
	}
	if err == nil {
		t.Fatalf("want error, connected (close: %v)", c.Close())
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("Dial took %s", d)
	}
	t.Logf("got %v after %s", err, time.Since(start))
}

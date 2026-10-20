//go:build windows

package hvsock

import (
	"crypto/rand"
	"encoding/binary"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var procConnect = ws2.NewProc("connect")

// loopbackPair accepts a connection from this partition to a fresh service.
func loopbackPair(t *testing.T) *Conn {
	t.Helper()
	var port [4]byte
	rand.Read(port[:])
	loopback, err := ParseGUID("e0e16197-dd56-4a10-9195-5ee7a155a838")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Listen(loopback, VsockServiceID(binary.LittleEndian.Uint32(port[:])))
	if err != nil {
		t.Skipf("hvsock loopback unavailable: %v", err)
	}
	closeSocket := func(fd syscall.Handle) {
		if err := syscall.Closesocket(fd); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { closeSocket(l.fd) })

	fd, err := syscall.Socket(afHyperV, syscall.SOCK_STREAM, hvProtocolRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSocket(fd) })
	if r, _, err := procConnect.Call(uintptr(fd), uintptr(unsafe.Pointer(&l.addr)), unsafe.Sizeof(l.addr)); r != 0 { //nolint:gosec // G103: SOCKADDR_HV for connect
		t.Skipf("hvsock loopback connect: %v", err)
	}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// bridge.Join closes a Conn while the other direction is blocked in Read.
func TestCloseWakesBlockedRead(t *testing.T) {
	c := loopbackPair(t)
	readErr := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		readErr <- err
	}()
	time.Sleep(100 * time.Millisecond)

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung on a blocked Read")
	}
	if err := <-readErr; err == nil {
		t.Fatal("blocked Read returned no error")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close succeeded")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

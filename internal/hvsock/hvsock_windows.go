//go:build windows

// Package hvsock is a minimal Hyper-V socket (AF_HYPERV) listener built on
// the standard library. I/O is blocking: agent traffic is tiny, so a thread
// per direction is simpler than driving overlapped I/O by hand.
package hvsock

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	afHyperV      = 34
	hvProtocolRaw = 1
	sdSend        = 1
)

var (
	ws2        = syscall.NewLazyDLL("ws2_32.dll")
	procBind   = ws2.NewProc("bind")
	procAccept = ws2.NewProc("accept")

	wsaStartup = sync.OnceValue(func() error {
		var d syscall.WSAData
		return syscall.WSAStartup(0x202, &d)
	})
)

// GUID is a Windows GUID in its in-memory layout.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// ParseGUID parses "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", optionally in braces.
func ParseGUID(s string) (GUID, error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	var g GUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return g, fmt.Errorf("invalid GUID %q", s)
	}
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil {
		return g, fmt.Errorf("invalid GUID %q", s)
	}
	g.Data1 = binary.BigEndian.Uint32(b[0:])
	g.Data2 = binary.BigEndian.Uint16(b[4:])
	g.Data3 = binary.BigEndian.Uint16(b[6:])
	copy(g.Data4[:], b[8:])
	return g, nil
}

func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x", g.Data1, g.Data2, g.Data3, g.Data4[:2], g.Data4[2:])
}

// VsockServiceID maps a Linux AF_VSOCK port to its Hyper-V service id.
func VsockServiceID(port uint32) GUID {
	return GUID{port, 0xfacb, 0x11e6, [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3}}
}

// Addr is SOCKADDR_HV.
type Addr struct {
	Family    uint16
	Reserved  uint16
	VMID      GUID
	ServiceID GUID
}

func (a *Addr) String() string { return a.VMID.String() + ":" + a.ServiceID.String() }

// Listener accepts Hyper-V socket connections.
type Listener struct {
	fd   syscall.Handle
	addr Addr
}

// Listen accepts connections from one VM to one service.
func Listen(vmID, serviceID GUID) (*Listener, error) {
	if err := wsaStartup(); err != nil {
		return nil, fmt.Errorf("WSAStartup: %w", err)
	}
	fd, err := syscall.Socket(afHyperV, syscall.SOCK_STREAM, hvProtocolRaw)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	l := &Listener{fd: fd, addr: Addr{Family: afHyperV, VMID: vmID, ServiceID: serviceID}}
	if r, _, err := procBind.Call(uintptr(fd), uintptr(unsafe.Pointer(&l.addr)), unsafe.Sizeof(l.addr)); r != 0 { //nolint:gosec // G103: SOCKADDR_HV for bind
		return nil, errors.Join(fmt.Errorf("bind %s: %w", &l.addr, err), syscall.Closesocket(fd))
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		return nil, errors.Join(fmt.Errorf("listen: %w", err), syscall.Closesocket(fd))
	}
	return l, nil
}

func (l *Listener) Addr() *Addr { return &l.addr }

func (l *Listener) Accept() (*Conn, error) {
	// No peer address: the listener admits a single VM, so it would say nothing.
	r, _, err := procAccept.Call(uintptr(l.fd), 0, 0)
	if syscall.Handle(r) == syscall.InvalidHandle {
		return nil, fmt.Errorf("accept: %w", err)
	}
	return &Conn{fd: syscall.Handle(r)}, nil
}

// Conn is an accepted connection. Close may be called concurrently with
// blocked Read/Write and more than once.
type Conn struct {
	fd syscall.Handle

	// Read/Write hold io shared for the whole syscall; Close releases the
	// handle only under io exclusively. Windows reuses handle values, so a
	// syscall racing closesocket could otherwise hit an unrelated socket.
	io        sync.RWMutex
	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// do runs op on the handle unless the connection is closing.
func (c *Conn) do(op func() error) error {
	c.io.RLock()
	defer c.io.RUnlock()
	if c.closing.Load() {
		return net.ErrClosed
	}
	return op()
}

func (c *Conn) Read(b []byte) (int, error) {
	var n uint32
	err := c.do(func() error {
		if len(b) == 0 {
			return nil
		}
		buf := wsaBuf(b)
		var flags uint32
		return syscall.WSARecv(c.fd, &buf, 1, &n, &flags, nil, nil)
	})
	if err == nil && n == 0 && len(b) > 0 {
		return 0, io.EOF
	}
	return int(n), err
}

func (c *Conn) Write(b []byte) (int, error) {
	written := 0
	err := c.do(func() error {
		for written < len(b) {
			buf := wsaBuf(b[written:])
			var n uint32
			if err := syscall.WSASend(c.fd, &buf, 1, &n, 0, nil, nil); err != nil {
				return err
			}
			written += int(n)
		}
		return nil
	})
	return written, err
}

// CloseWrite sends FIN while keeping the read side open.
func (c *Conn) CloseWrite() error {
	return c.do(func() error { return syscall.Shutdown(c.fd, sdSend) })
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		// shutdown() does not wake a blocked recv, CancelIoEx does. Keep
		// cancelling until every in-flight call has let go of the handle:
		// one may have passed the closing check just before we set it.
		for !c.io.TryLock() {
			// ERROR_NOT_FOUND here only means nothing was pending yet.
			_ = syscall.CancelIoEx(c.fd, nil)
			time.Sleep(time.Millisecond)
		}
		c.closeErr = syscall.Closesocket(c.fd)
		c.io.Unlock()
	})
	return c.closeErr
}

// wsaBuf describes b, capped at what one WSARecv/WSASend call can take.
func wsaBuf(b []byte) syscall.WSABuf {
	return syscall.WSABuf{Len: uint32(min(len(b), math.MaxInt32)), Buf: &b[0]}
}

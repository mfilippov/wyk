//go:build linux

// Package vsock dials AF_VSOCK using only the standard library.
package vsock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	afVsock = 40
	// CIDHost is VMADDR_CID_HOST; under Hyper-V it reaches the parent partition.
	CIDHost = 2
)

// sockaddrVM is struct sockaddr_vm.
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

// Conn is a connected vsock stream backed by the runtime poller.
type Conn struct {
	*os.File
}

// Dial connects to cid:port, giving up after timeout.
//
// The connect is non-blocking and waits in the runtime poller, as net does:
// a blocking connect interrupted by a signal (EINTR) keeps going in the
// kernel, so it cannot simply be retried.
func Dial(cid, port uint32, timeout time.Duration) (*Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	fail := func(err error) (*Conn, error) {
		return nil, errors.Join(fmt.Errorf("connect vsock %d:%d: %w", cid, port, err), f.Close())
	}

	sa := sockaddrVM{Family: afVsock, Port: port, CID: cid}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)) //nolint:gosec // G103: sockaddr_vm for connect
	switch errno {
	case 0:
		return &Conn{f}, nil
	case syscall.EINPROGRESS, syscall.EINTR:
	default:
		return fail(errno)
	}

	// Writable means the connect finished; SO_ERROR tells how.
	rc, err := f.SyscallConn()
	if err != nil {
		return fail(err)
	}
	// Without a deadline the wait below could last forever.
	if err := f.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fail(err)
	}
	var soErr error
	waited := false
	err = rc.Write(func(fd uintptr) bool {
		if !waited {
			waited = true
			return false
		}
		v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
		switch {
		case err != nil:
			soErr = err
		case v != 0:
			soErr = syscall.Errno(v)
		}
		return true
	})
	if err := errors.Join(err, soErr, f.SetWriteDeadline(time.Time{})); err != nil {
		return fail(err)
	}
	return &Conn{f}, nil
}

// CloseWrite sends FIN while keeping the read side open.
func (c *Conn) CloseWrite() error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Shutdown(int(fd), syscall.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

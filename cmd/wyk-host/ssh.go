//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/mfilippov/wyk/internal/bridge"
)

// oFileFlagOverlapped makes os.OpenFile open the handle for overlapped I/O
// so it joins the runtime poller. On a synchronous pipe handle a pending
// ReadFile blocks every WriteFile, which deadlocks a duplex copy.
const oFileFlagOverlapped = 0x40000000

// sshAgentPipe is where gpg-agent serves ssh-agent with enable-win32-openssh-support.
const sshAgentPipe = `\\.\pipe\openssh-ssh-agent`

const errorPipeBusy = syscall.Errno(231)

// pipeConn adapts a named pipe to bridge.Conn. Pipes cannot half-close, and
// the relay cannot tell a client's half-close from a full close, so
// CloseWrite closes the pipe. ssh-agent clients wait for each reply before
// closing; a client that half-closes right after a request loses the reply.
type pipeConn struct {
	*os.File
}

func (p pipeConn) CloseWrite() error { return p.Close() }

func dialPipe(name string) (bridge.Conn, error) {
	for i := 0; ; i++ {
		f, err := os.OpenFile(name, os.O_RDWR|oFileFlagOverlapped, 0) //nolint:gosec // G304: a fixed pipe name
		if err == nil {
			return pipeConn{f}, nil
		}
		// All server instances are taken; gpg-agent creates a new one shortly.
		if !errors.Is(err, errorPipeBusy) || i == 20 {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

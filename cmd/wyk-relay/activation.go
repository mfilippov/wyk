//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// listenFDsStart is SD_LISTEN_FDS_START.
const listenFDsStart = 3

// activationListeners returns the sockets passed by systemd socket
// activation, keyed by FileDescriptorName. It returns nil when the process
// was not socket-activated.
func activationListeners() (map[string]*net.UnixListener, error) {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil {
		return nil, fmt.Errorf("LISTEN_FDS: %w", err)
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	// Keep these away from wyk-host and anything else we spawn.
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if err := os.Unsetenv(v); err != nil {
			return nil, err
		}
	}

	ls := make(map[string]*net.UnixListener, n)
	for i := 0; i < n; i++ {
		fd := listenFDsStart + i
		syscall.CloseOnExec(fd)
		name := "unknown"
		if i < len(names) {
			name = names[i]
		}
		f := os.NewFile(uintptr(fd), name)
		l, err := net.FileListener(f) // dups the fd
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, fmt.Errorf("socket %q: %w", name, err)
		}
		ul, ok := l.(*net.UnixListener)
		if !ok {
			return nil, fmt.Errorf("socket %q is not a unix socket", name)
		}
		ls[name] = ul
	}
	return ls, nil
}

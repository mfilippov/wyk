//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/mfilippov/wyk/internal/bridge"
)

const nonceSize = 16

// extraSocket is the only gpg-agent socket wyk ever forwards. The extra
// socket is gpg-agent's restricted one for remote use: signing and
// decryption work, key and card management (PIN changes, card reset) do
// not. WSL is reachable from containers and every distro, so it gets no more.
// The path does not depend on a running agent, so the first one found is
// kept; a failure (gpgconf mid-upgrade, say) is retried next time.
func extraSocket() (string, error) {
	extra.Lock()
	defer extra.Unlock()
	if extra.path != "" {
		return extra.path, nil
	}
	out, err := gpgconf("--list-dirs", "agent-extra-socket")
	if err != nil {
		return "", err
	}
	// gpgconf percent-escapes ':' and '%' in paths.
	path, err := url.PathUnescape(strings.TrimSpace(string(out)))
	if err == nil && path == "" {
		err = errors.New("gpgconf reports no agent-extra-socket")
	}
	if err != nil {
		return "", err
	}
	extra.path = path
	return path, nil
}

var extra struct {
	sync.Mutex
	path string
}

// gpgconf runs gpgconf without a console window; a failure carries its stderr.
func gpgconf(args ...string) ([]byte, error) {
	cmd := exec.Command("gpgconf", args...) //nolint:gosec // G204: gpgconf with our own constant arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exit.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("gpgconf %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// dialAssuan connects to a libassuan emulated socket: the file holds the
// TCP port on 127.0.0.1, a newline and a 16-byte nonce sent as the first bytes.
func dialAssuan(path string) (bridge.Conn, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: gpgconf's own socket path
	if err != nil {
		return nil, err
	}
	port, nonce, ok := bytes.Cut(b, []byte("\n"))
	if !ok || len(nonce) != nonceSize {
		return nil, fmt.Errorf("%s: not an assuan socket file", path)
	}
	if _, err := strconv.ParseUint(string(port), 10, 16); err != nil {
		return nil, fmt.Errorf("%s: bad port %q", path, port)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", string(port)))
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(nonce); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn.(*net.TCPConn), nil
}

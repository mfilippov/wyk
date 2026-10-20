//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The systemd user units install writes and uninstall removes, all under
// ~/.config/systemd/user: wyk's two sockets and service, and a drop-in per
// distro gpg-agent socket that keeps it from starting.

// Distro packaging ships these; they would race us for the gpg socket path.
var gpgAgentUnits = []string{
	"gpg-agent.socket",
	"gpg-agent-extra.socket",
	"gpg-agent-ssh.socket",
	"gpg-agent-browser.socket",
}

// skipDistroAgent is a drop-in that keeps a distro gpg-agent socket from
// starting while wyk is installed. Unlike a mask, uninstall can remove it
// without touching a mask the user set themselves.
const skipDistroAgent = "[Unit]\n# wyk serves the gpg-agent socket; wyk-host.exe uninstall removes this file.\nConditionPathExists=!/dev/null\n"

// dropIn is the path of our drop-in for unit, relative to the unit dir.
func dropIn(unit string) string { return filepath.Join(unit+".d", "wyk.conf") }

const (
	unitService   = "wyk-relay.service"
	unitSSHSocket = "wyk-ssh.socket"
	unitGPGSocket = "wyk-gpg.socket"
)

func writeUnits(p *paths, gpgSock string) error {
	units := map[string]string{
		unitService: fmt.Sprintf(`[Unit]
Description=wyk: SSH and GPG agents from Windows (YubiKey)
Documentation=https://github.com/mfilippov/wyk
%s%s
# The sockets start the relay per request. A relay that fails (host
# unreachable) must not trip the start limit, which would disable the
# sockets for good; it refuses queued requests, so each start is one.
StartLimitIntervalSec=0

# No Restart=: the sockets start the relay again on the next request.
[Service]
ExecStart=%s -host %s
`, hostRecord, unitQuote(p.hostExe), unitQuote(p.relayBin), unitQuote(p.hostExe)),
		unitSSHSocket: `[Unit]
Description=wyk: SSH agent socket

[Socket]
ListenStream=%t/` + sshSock + `
FileDescriptorName=ssh
Service=wyk-relay.service
SocketMode=0600
DirectoryMode=0700

[Install]
WantedBy=sockets.target
`,
		unitGPGSocket: fmt.Sprintf(`[Unit]
Description=wyk: GnuPG agent socket

[Socket]
ListenStream=%s
FileDescriptorName=gpg
Service=wyk-relay.service
SocketMode=0600
DirectoryMode=0700

[Install]
WantedBy=sockets.target
`, unitEscape(gpgSock)),
	}
	for _, u := range gpgAgentUnits {
		units[dropIn(u)] = skipDistroAgent
	}
	for name, body := range units {
		path := filepath.Join(p.unitDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// unitEscape protects a value from systemd's %-specifier expansion.
func unitEscape(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// unitQuote makes s a single ExecStart= word: quoted, with the C-style
// escapes, %-specifiers and $VARIABLES systemd would otherwise interpret
// neutralized. unitUnquote reverses it.
func unitQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

func unitUnquote(q string) (string, error) {
	s, err := strconv.Unquote(q)
	return strings.NewReplacer("%%", "%", "$$", "$").Replace(s), err
}

// distroUnits returns which of names this distro ships (loaded) and which
// of those the user left enabled. A missing unit has LoadState "not-found",
// a masked one "masked", a disabled one UnitFileState "disabled".
func distroUnits(names []string) (loaded, enabled []string, err error) {
	out, err := output("systemctl", append([]string{"--user", "show", "--property=Id,LoadState,UnitFileState"}, names...)...)
	if err != nil {
		return nil, nil, err
	}
	for _, block := range strings.Split(string(out), "\n\n") {
		props := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(line, "=")
			props[k] = v
		}
		if props["LoadState"] != "loaded" {
			continue
		}
		loaded = append(loaded, props["Id"])
		// "static": wanted through a vendor link, which these units with an
		// [Install] section only get to be started at login.
		if s := props["UnitFileState"]; strings.HasPrefix(s, "enabled") || s == "static" {
			enabled = append(enabled, props["Id"])
		}
	}
	return loaded, enabled, nil
}

// systemctlOn runs systemctl verb on units, if there are any.
func systemctlOn(verb string, units []string) error {
	if len(units) == 0 {
		return nil
	}
	return systemctl(slices.Concat([]string{verb}, units)...)
}

func systemctl(args ...string) error {
	_, err := output("systemctl", append([]string{"--user"}, args...)...)
	return err
}

// hostRecord marks the comment line in the service unit that tells
// uninstall where install put wyk-host.exe.
const hostRecord = "# wyk-host.exe: "

// recordedHost reads back the quoted path on the unit's hostRecord line.
func recordedHost(unitDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(unitDir, unitService)) //nolint:gosec // G304: the unit install wrote
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if q, ok := strings.CutPrefix(line, hostRecord); ok {
			s, err := unitUnquote(q)
			if err != nil {
				return "", fmt.Errorf("%s: %s%s: %w", unitService, hostRecord, q, err)
			}
			return s, nil
		}
	}
	return "", fmt.Errorf("%s records no %q", unitService, strings.TrimSpace(hostRecord))
}

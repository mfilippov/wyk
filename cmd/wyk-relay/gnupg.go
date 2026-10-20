//go:build linux

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// The local GnuPG: where its agent socket is, and the public keys copied
// from the Windows keyring. Only checked hex fingerprints reach gpg.exe.

// importWindowsKeys copies the public keys (and their ownertrust) of every
// Windows secret key into the local keyring, so local gpg can use the
// forwarded agent for signing and decryption.
//
// Interop flattens arguments into one Windows command line, which gpg.exe
// splits again by its own rules, so only strict hex fingerprints go there.
func importWindowsKeys(winDir string) error {
	// gpg.exe may start the Windows gpg-agent, which keeps the working
	// directory for life: give it winDir, not \wsl.localhost...
	gpgExe := func(args ...string) ([]byte, error) {
		cmd := exec.Command("gpg.exe", args...) //nolint:gosec // G204: no shell; fixed flags and checked fingerprints
		cmd.Dir = winDir
		return runCmd(cmd)
	}
	out, err := gpgExe("--batch", "--with-colons", "--list-secret-keys")
	if err != nil {
		return err
	}
	var fprs []string
	afterSec := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), ":")
		switch {
		case f[0] == "sec":
			afterSec = true
		case f[0] == "fpr" && afterSec && len(f) > 9 && isFingerprint(f[9]):
			fprs = append(fprs, f[9])
			afterSec = false
		}
	}
	if len(fprs) == 0 {
		fmt.Println("       no secret keys in the Windows keyring")
		return nil
	}

	pub, err := gpgExe(append([]string{"--batch", "--export"}, fprs...)...) // fingerprints checked by isFingerprint
	if err != nil {
		return err
	}
	imp := exec.Command("gpg", "--batch", "--import")
	imp.Stdin = bytes.NewReader(pub)
	if _, err := runCmd(imp); err != nil {
		return err
	}

	trust, err := gpgExe("--batch", "--export-ownertrust")
	if err != nil {
		return err
	}
	var mine bytes.Buffer
	for _, line := range strings.Split(string(trust), "\n") {
		line = strings.TrimRight(line, "\r")
		for _, fpr := range fprs {
			if strings.HasPrefix(line, fpr+":") {
				mine.WriteString(line + "\n")
			}
		}
	}
	if mine.Len() > 0 {
		it := exec.Command("gpg", "--batch", "--import-ownertrust")
		it.Stdin = &mine
		if _, err := runCmd(it); err != nil {
			return err
		}
	}
	fmt.Printf("       imported %s\n", strings.Join(fprs, ", "))
	return nil
}

// isFingerprint accepts a v4 (40) or v5 (64) hex OpenPGP fingerprint.
func isFingerprint(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && (len(s) == 40 || len(s) == 64)
}

// gpgAgentSocket is where the local gpg looks for its agent, so gpg talks to
// the Windows agent without any client configuration.
func gpgAgentSocket() (string, error) {
	out, err := output("gpgconf", "--list-dirs", "agent-socket")
	if err != nil {
		return "", fmt.Errorf("%w (is gnupg installed?)", err)
	}
	// gpgconf percent-escapes ':' and '%' in paths.
	return url.PathUnescape(strings.TrimSpace(string(out)))
}

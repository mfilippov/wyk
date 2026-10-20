//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// sshSock is where wyk-ssh.socket listens, under the user manager's %t,
// which is always /run/user/UID.
const sshSock = "wyk/ssh.sock"

type paths struct {
	home     string
	unitDir  string
	relayBin string
	hostExe  string // as a Linux path
}

func linuxPaths() *paths {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return &paths{
		home:     home,
		unitDir:  filepath.Join(home, ".config", "systemd", "user"),
		relayBin: filepath.Join(home, ".local", "bin", "wyk-relay"),
	}
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	hostSrc := fs.String("host", "", "wyk-host.exe to install, as a Linux path")
	noKeys := fs.Bool("no-import", false, "do not import public keys from the Windows keyring")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	if *hostSrc == "" {
		log.Fatal("install from Windows: run the downloaded wyk exe with install [-d distro]")
	}

	p := linuxPaths()
	var err error
	var localAppData string
	if p.hostExe, localAppData, err = hostInstallPath(*hostSrc); err != nil {
		log.Fatal(err)
	}
	gpgSock, err := gpgAgentSocket()
	if err != nil {
		log.Fatal(err)
	}
	name := strconv.Itoa(os.Getuid())
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	fmt.Printf("Installing wyk %s for %s in WSL distro %s\n", version, name, os.Getenv("WSL_DISTRO_NAME"))

	// Units first: they record where everything goes, so uninstall can clean
	// up after any later failure, and they need systemd, which may be off.
	step("write systemd units to "+p.unitDir, func() error {
		if err := writeUnits(p, gpgSock); err != nil {
			return err
		}
		return systemctl("daemon-reload")
	})
	step("stop gpg-agent", func() error {
		// wyk first, on a reinstall: its binaries are replaced next, and
		// gpgconf --kill below must reach the distro agent, not the Windows
		// one through wyk's socket. Every loaded distro socket goes too: a
		// disabled one may still be running.
		loaded, _, err := distroUnits(gpgAgentUnits)
		if err == nil {
			err = systemctlOn("stop", append([]string{unitService, unitSSHSocket, unitGPGSocket}, loaded...))
		}
		if err != nil {
			return err
		}
		// Only the agent of this GNUPGHOME; agents of other homes keep running.
		_, err = output("gpgconf", "--kill", "gpg-agent")
		return err
	})
	step("install "+p.hostExe, func() error { return installFile(*hostSrc, p.hostExe, 0o755) })
	step("install "+p.relayBin, func() error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		return installFile(self, p.relayBin, 0o755)
	})
	// The next request starts the relay, the new binary on a reinstall.
	step("enable "+unitSSHSocket+" "+unitGPGSocket, func() error {
		return systemctl("enable", "--now", unitSSHSocket, unitGPGSocket)
	})
	step("SSH_AUTH_SOCK in shell startup files", func() error {
		for _, f := range shellFiles(p.home, false) {
			if err := upsertBlock(f, shellBlock, 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if !*noKeys {
		// Forwarding already works without this; a failure only means the
		// user has to import their public key by hand.
		// In %LOCALAPPDATA%: a gpg-agent started here keeps it as working
		// directory, and wyk never removes it.
		if err := importWindowsKeys(localAppData); err != nil {
			fmt.Printf("  warn import public keys from Windows: %v\n", err)
		} else {
			fmt.Println("  ok   import public keys from Windows")
		}
	}
	// The shell block keeps an SSH_AUTH_SOCK that is already set, which may
	// point at the distro agent just stopped (Debian's gpg-agent sets it).
	ours := fmt.Sprintf("/run/user/%d/%s", os.Getuid(), sshSock)
	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" && s != ours {
		fmt.Printf("  warn SSH_AUTH_SOCK is %s, set outside wyk; remove that setting to use %s\n", s, ours)
	}
	// Other shells read neither file; their config is theirs to edit.
	if sh := filepath.Base(os.Getenv("SHELL")); os.Getenv("SHELL") != "" && sh != "bash" && sh != "sh" && sh != "dash" {
		fmt.Printf("  warn your shell is %s: add export SSH_AUTH_SOCK=%s to its startup file\n", sh, ours)
	}
	fmt.Println("\nDone. Open a new WSL shell, then try: ssh-add -L; gpg -K")
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	p := linuxPaths()
	hostExe, hostErr := recordedHost(p.unitDir)
	// Enabled wyk sockets must be stopped before their binaries go, or they
	// would come back at the next login pointing at nothing, so with them a
	// systemd failure stops uninstall. An install that never enabled them
	// (no systemd, say) only gets such failures reported.
	wykEnabled := false
	for _, u := range []string{unitSSHSocket, unitGPGSocket} {
		if _, err := os.Stat(filepath.Join(p.unitDir, "sockets.target.wants", u)); err == nil {
			wykEnabled = true
		}
	}
	systemd := func(err error) error {
		if err != nil && !wykEnabled {
			fmt.Printf("  warn %v\n", err)
			return nil
		}
		return err
	}
	step("stop wyk", func() error {
		return systemd(errors.Join(systemctl("disable", "--now", unitSSHSocket, unitGPGSocket), systemctl("stop", unitService)))
	})
	// Right after wyk stops: the steps below can fail (a dotfile edited by
	// hand, a Windows exe held by a scanner), and gpg must not be left
	// without an agent meanwhile.
	step("restore distro gpg-agent sockets", func() error {
		for _, u := range gpgAgentUnits {
			f := filepath.Join(p.unitDir, dropIn(u))
			if err := errors.Join(removeIfExists(f), removeDirIfEmpty(filepath.Dir(f))); err != nil {
				return err
			}
		}
		if err := systemd(systemctl("daemon-reload")); err != nil {
			return err
		}
		_, enabled, err := distroUnits(gpgAgentUnits)
		if err == nil {
			err = systemctlOn("start", enabled)
		}
		return systemd(err)
	})
	step("remove wyk blocks from shell startup files", func() error {
		for _, f := range shellFiles(p.home, true) {
			if err := removeBlock(f); err != nil {
				return err
			}
		}
		return nil
	})
	step("remove "+p.relayBin, func() error { return removeIfExists(p.relayBin) })
	if errors.Is(hostErr, os.ErrNotExist) {
		// No unit: never installed, or already uninstalled.
	} else if hostErr != nil {
		fmt.Printf("  warn wyk-host.exe location unknown, remove it by hand: %v\n", hostErr)
	} else {
		step("remove "+hostExe, func() error { return removeHost(hostExe) })
	}
	// Last: the service unit records what uninstall has to find, so a rerun
	// after any failure above still finds it.
	step("remove wyk units", func() error {
		for _, u := range []string{unitService, unitSSHSocket, unitGPGSocket} {
			if err := removeIfExists(filepath.Join(p.unitDir, u)); err != nil {
				return err
			}
		}
		// systemd made sockets.target.wants when wyk's sockets were enabled.
		wants := filepath.Join(p.unitDir, "sockets.target.wants")
		return errors.Join(removeDirIfEmpty(wants), removeDirIfEmpty(p.unitDir), systemd(systemctl("daemon-reload")))
	})
	fmt.Println("\nDone. Imported public keys were left in the keyring.")
}

func step(name string, fn func() error) {
	if err := fn(); err != nil {
		fmt.Printf("  FAIL %s: %v\n", name, err)
		os.Exit(1)
	}
	fmt.Printf("  ok   %s\n", name)
}

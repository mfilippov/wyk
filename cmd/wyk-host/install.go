//go:build windows

package main

import (
	"embed"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// relay holds wyk-relay for this exe's architecture, put there by
// scripts/build.sh; a plain go build finds only the README.
//
//go:embed relay
var relay embed.FS

// relayScript stages the relay in the distro and runs its subcommand.
//
//go:embed relay.sh
var relayScript string

// targetFlags adds the flags that pick where to act: -d distro, -u user.
func targetFlags(fs *flag.FlagSet) (distro, user *string) {
	return fs.String("d", "", "WSL distro (default: WSL's default distro)"),
		fs.String("u", "", "user in the distro (default: the distro's default user)")
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	distro, user := targetFlags(fs)
	noKeys := fs.Bool("no-import", false, "do not import public keys from the Windows keyring")
	parse(fs, args)
	var flags []string
	if *noKeys {
		flags = append(flags, "-no-import")
	}
	runRelay(*distro, *user, "install", flags...)
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	distro, user := targetFlags(fs)
	parse(fs, args)
	// Windows cannot delete a running exe, so the copy install made would
	// stop uninstall halfway, with the units still in place.
	if installedCopy() {
		fail("this is the copy wyk installed, which cannot remove itself: run the downloaded exe, or copy this one elsewhere first")
	}
	runRelay(*distro, *user, "uninstall")
}

// installedCopy reports whether this exe runs from under %LOCALAPPDATA%\wyk.
// Folders are compared as files, not paths, which may be spelled with 8.3
// names or through a junction.
func installedCopy() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	local, err := os.UserCacheDir()
	if err != nil {
		return false
	}
	wyk, err := os.Stat(filepath.Join(local, "wyk"))
	if err != nil {
		return false
	}
	for dir := filepath.Dir(self); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, wyk) {
			return true
		}
	}
	return false
}

// parse also refuses leftover words: "install Debian" without -d would
// otherwise act on the default distro.
func parse(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		quit(0)
	} else if err != nil {
		quit(2) // the FlagSet has printed the error and its flags
	}
	if fs.NArg() > 0 {
		fail("%s: unexpected %q (a distro goes after -d)", fs.Name(), fs.Arg(0))
	}
}

// runRelay runs the embedded relay's subcommand in the distro, as user or
// else the distro's default user, and exits with its status; the distro
// side has already said what went wrong.
func runRelay(distro, user, subcmd string, flags ...string) {
	bin, err := relay.Open("relay/wyk-relay")
	if err != nil {
		fail("built without wyk-relay inside: build with scripts/build.sh")
	}
	// Interop does not tell a Windows program which distro and user it came
	// from, so WSL's defaults would quietly stand in for them.
	if (distro == "" || user == "") && startedFromWSL() {
		fail(`started from inside WSL, which does not say from which distro and user: add -d "$WSL_DISTRO_NAME" -u "$USER"`)
	}
	self, err := os.Executable()
	if err != nil {
		fail("%v", err)
	}
	machine := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	args := []string{}
	if distro != "" {
		args = append(args, "-d", distro)
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	// wsl.exe hands argv over unchanged; see relay.sh for what each is.
	args = append(args, "--exec", "sh", "-c", relayScript, "sh", self, machine, version, subcmd)
	cmd := exec.Command("wsl.exe", append(args, flags...)...) //nolint:gosec // G204: no shell on this side; values reach the script as arguments
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")              // its own messages, else UTF-16
	cmd.Stdin = bin
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		quit(exit.ExitCode())
	}
	if err != nil {
		fail("%v", err)
	}
	quit(0)
}

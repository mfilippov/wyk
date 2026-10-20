//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The binaries install copies and uninstall deletes: wyk-relay in
// ~/.local/bin, and the copy of wyk-host.exe in
// %LOCALAPPDATA%\wyk\<distro>\<uid>\, reached through /mnt. That copy and
// its folders are the only things wyk writes outside the user's home.

// installFile copies src to dst via a temporary file. A running Windows
// executable cannot be overwritten but can be renamed, so the old one is
// moved aside first; deleting it may fail while it runs, which is harmless.
func installFile(src, dst string, mode os.FileMode) (err error) {
	if same, err := sameFile(src, dst); err != nil || same {
		return err
	}
	data, err := os.ReadFile(src) //nolint:gosec // G304: the wyk binary being installed
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".new"
	defer func() {
		if err != nil { // no half-written copy left behind
			err = errors.Join(err, removeIfExists(tmp))
		}
	}()
	if err := os.WriteFile(tmp, data, mode); err != nil { //nolint:gosec // G703: next to our own install target
		return err
	}
	old := dst + ".old"
	if err := removeIfExists(old); err != nil {
		return fmt.Errorf("%w (is an older wyk-host still running?)", err)
	}
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	// Deleting fails while the old Windows exe still runs; the new wyk-host
	// removes its .old when it starts.
	_ = removeIfExists(old)
	return nil
}

// sameFile reports whether b exists and is the same file as a.
func sameFile(a, b string) (bool, error) {
	sb, err := os.Stat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// hostInstallPath is where this user's wyk-host.exe goes, as a Linux path:
// %LOCALAPPDATA%\wyk\<distro>\<uid>\, with %LOCALAPPDATA% as wyk-host.exe
// reports it. Each distro and user gets its own copy, so uninstalling or
// upgrading wyk for one cannot pull the exe from under another's host. The
// uid, unlike a user name, cannot clash in a case-insensitive folder name.
func hostInstallPath(hostSrc string) (exe, localAppData string, err error) {
	distro := os.Getenv("WSL_DISTRO_NAME")
	if !distroPattern.MatchString(distro) {
		return "", "", fmt.Errorf("WSL_DISTRO_NAME %q: not a WSL distro, or not a name wyk can use as a Windows folder", distro)
	}
	out, err := output(hostSrc, "localappdata")
	if err != nil {
		return "", "", fmt.Errorf("%w (is WSL interop enabled?)", err)
	}
	lin, err := output("wslpath", "-u", strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", err
	}
	localAppData = strings.TrimSpace(string(lin))
	return filepath.Join(localAppData, "wyk", distro, strconv.Itoa(os.Getuid()), "wyk-host.exe"), localAppData, nil
}

// A folder name, and never "." or "..", nor one ending in a dot, which
// Windows drops ("Ubuntu." would share the folder of "Ubuntu").
var distroPattern = regexp.MustCompile(`^[A-Za-z0-9_-]([A-Za-z0-9._-]*[A-Za-z0-9_-])?$`)

// removeHost deletes wyk-host.exe, its upgrade leftovers and then its folder
// if that is empty. The path comes from a file, so nothing is removed
// recursively. The host exits once the relay stops, which takes a moment.
func removeHost(exe string) error {
	if filepath.Base(exe) != "wyk-host.exe" {
		return fmt.Errorf("refusing to remove %s", exe)
	}
	err := removeIfExists(exe)
	for i := 0; err != nil && i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		err = removeIfExists(exe)
	}
	if err != nil {
		return err
	}
	for _, f := range []string{exe + ".old", exe + ".new"} {
		if err := removeIfExists(f); err != nil {
			return err
		}
	}
	// This user's folder, the distro's, then the shared wyk one. A folder
	// that is not empty keeps those above it from being empty too. One a
	// Windows process works in cannot go, but that must not stop uninstall.
	for dir, i := filepath.Dir(exe), 0; i < 3; dir, i = filepath.Dir(dir), i+1 {
		err := os.Remove(dir)
		if errors.Is(err, syscall.ENOTEMPTY) {
			return nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Printf("  warn %v (in use? remove it by hand)\n", err)
			return nil
		}
	}
	return nil
}

func removeDirIfEmpty(dir string) error {
	if err := os.Remove(dir); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

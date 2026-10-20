//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const bashrc = "# ~/.bashrc\nalias ll='ls -l'\n"

// writeFile and readFile fail the test on any error, so a broken setup can
// never let an assertion pass on the wrong file.
func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: want it gone, stat says %v", path, err)
	}
}

func TestBlockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bashrc")
	writeFile(t, path, bashrc, 0o640)

	if err := upsertBlock(path, shellBlock, 0o644); err != nil {
		t.Fatal(err)
	}
	once := readFile(t, path)
	if !strings.HasPrefix(once, bashrc) || strings.Count(once, blockBegin) != 1 {
		t.Fatalf("after install:\n%s", once)
	}

	// Reinstall replaces the block instead of stacking another one.
	if err := upsertBlock(path, shellBlock, 0o644); err != nil {
		t.Fatal(err)
	}
	if twice := readFile(t, path); twice != once {
		t.Fatalf("upsert is not idempotent:\n%s", twice)
	}

	if err := removeBlock(path); err != nil {
		t.Fatal(err)
	}
	if back := readFile(t, path); back != bashrc {
		t.Fatalf("uninstall did not restore the file:\n%q", back)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %o", st.Mode().Perm())
	}
}

// Dotfile managers link ~/.bashrc elsewhere; the link must survive.
func TestBlockWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "dotfiles-bashrc"), filepath.Join(dir, ".bashrc")
	writeFile(t, target, bashrc, 0o640)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := upsertBlock(link, shellBlock, 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a symlink: %v", link, err)
	}
	if got := readFile(t, target); !strings.Contains(got, blockBegin) {
		t.Fatalf("target not updated: %q", got)
	}
	mustNotExist(t, target+".wyk-new")
}

// A strict umask must not change the mode of the file we rewrite.
func TestBlockKeepsModeUnderUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".profile")
	writeFile(t, path, bashrc, 0o600)
	if err := os.Chmod(path, 0o664); err != nil { //nolint:gosec // G302: the wider mode is what the test checks
		t.Fatal(err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if err := upsertBlock(path, shellBlock, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o664 {
		t.Fatalf("mode changed to %o", st.Mode().Perm())
	}
}

// A link to a dotfile that is not there yet stays a link; we refuse to write.
func TestBlockRefusesDanglingSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
		t.Fatal(err)
	}
	if err := upsertBlock(link, shellBlock, 0o644); err == nil {
		t.Fatal("wrote through a dangling symlink")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v", err)
	}
}

// Removing our block from a linked file that held only it keeps the link.
func TestRemoveBlockKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "dotfiles-gpg.conf"), filepath.Join(dir, "gpg.conf")
	writeFile(t, target, "", 0o600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := upsertBlock(link, "export X=1", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeBlock(link); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link removed: %v", err)
	}
	if got := readFile(t, target); strings.TrimSpace(got) != "" {
		t.Fatalf("target still holds %q", got)
	}
}

func TestBlockMissingTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpg.conf")
	writeFile(t, path, "keyid-format long", 0o600)
	if err := upsertBlock(path, "export X=1", 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.HasPrefix(got, "keyid-format long\n"+blockBegin+"\n") {
		t.Fatalf("got %q", got)
	}
}

func TestBlockKeepsEveryByte(t *testing.T) {
	long := strings.Repeat("x", 100<<10) // longer than bufio.Scanner's default limit
	for _, orig := range []string{
		"",
		"no trailing newline",
		"a\n" + long + "\nb\n",
		"a\r\nb\r\n",
	} {
		path := filepath.Join(t.TempDir(), "rc")
		writeFile(t, path, orig, 0o600)
		if err := upsertBlock(path, shellBlock, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := removeBlock(path); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != orig && got != orig+"\n" {
			t.Fatalf("round trip of %.40q gave %.40q", orig, got)
		}
	}
}

func TestBlockInTheMiddle(t *testing.T) {
	in := "a\n" + blockBegin + "\nold\n" + blockEnd + "\nb"
	got, err := stripBlock([]byte(in))
	if err != nil || string(got) != "a\nb" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Without an end marker we cannot know where our block stops; touching the
// file could drop the user's lines.
func TestUnterminatedBlockIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bashrc")
	orig := "a\n" + blockBegin + "\nexport X=1\nuser line\n"
	writeFile(t, path, orig, 0o600)
	if err := upsertBlock(path, shellBlock, 0o644); err == nil {
		t.Fatal("upsert accepted an unterminated block")
	}
	if err := removeBlock(path); err == nil {
		t.Fatal("remove accepted an unterminated block")
	}
	if got := readFile(t, path); got != orig {
		t.Fatalf("file was modified: %q", got)
	}
}

func TestUnitQuote(t *testing.T) {
	if got := unitQuote(`/mnt/c/Users/A "B" 100%\x/wyk-host.exe`); got != `"/mnt/c/Users/A \"B\" 100%%\\x/wyk-host.exe"` {
		t.Fatalf("got %s", got)
	}
	if got := unitEscape("/run/user/1000/gnupg/50%/S.gpg-agent"); got != "/run/user/1000/gnupg/50%%/S.gpg-agent" {
		t.Fatalf("got %s", got)
	}
}

// uninstall finds wyk-host.exe through the unit install wrote; Windows user
// names can hold any of these.
func TestHostPathSurvivesUnitFile(t *testing.T) {
	p := &paths{unitDir: t.TempDir(), relayBin: "/home/u/.local/bin/wyk-relay"}
	for _, path := range []string{
		"/mnt/c/Users/u/AppData/Local/wyk/wyk-host.exe",
		`/mnt/c/Users/Михаил "100%" \ x/AppData/Local/wyk/wyk-host.exe`,
		`/mnt/c/Users/dev$x ${HOME}/AppData/Local/wyk/wyk-host.exe`,
	} {
		p.hostExe = path
		if err := writeUnits(p, "/run/user/1000/gnupg/S.gpg-agent"); err != nil {
			t.Fatal(err)
		}
		got, err := recordedHost(p.unitDir)
		if err != nil || got != path {
			t.Fatalf("got %q, %v; want %q", got, err, path)
		}
	}
}

func TestRemoveHostRefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "important.txt")
	writeFile(t, other, "", 0o600)
	if err := removeHost(other); err == nil {
		t.Fatal("removeHost accepted a file that is not wyk-host.exe")
	}
	exe := filepath.Join(dir, "wyk-host.exe")
	writeFile(t, exe, "", 0o600)
	if err := removeHost(exe); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, exe)
	readFile(t, other) // the neighbour survives
}

func TestRemoveHostKeepsOtherUsersAndCleansUp(t *testing.T) {
	wyk := filepath.Join(t.TempDir(), "wyk")
	first := filepath.Join(wyk, "Ubuntu", "1000", "wyk-host.exe")
	second := filepath.Join(wyk, "Ubuntu", "1001", "wyk-host.exe")
	for _, f := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, f, "", 0o600)
	}
	if err := removeHost(first); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Dir(first))
	readFile(t, second)
	if err := removeHost(second); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, wyk)
}

// The distro name becomes a folder name under %LOCALAPPDATA%\wyk.
func TestDistroPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"Ubuntu":       true,
		"Ubuntu-24.04": true,
		"my_distro":    true,
		"a":            true,
		"_dev":         true,
		".hidden":      false,
		"Ubuntu.":      false,
		"":             false,
		".":            false,
		"..":           false,
		"a/b":          false,
		`a\b`:          false,
	} {
		if distroPattern.MatchString(s) != want {
			t.Errorf("distroPattern(%q) != %v", s, want)
		}
	}
}

// Values passed to Windows programs must not be able to hold separators.
func TestInteropArgumentFormats(t *testing.T) {
	for s, want := range map[string]bool{
		"59145519-2C9D-4FEC-8CAE-4206ED93CBA4":           true,
		"59145519-2c9d-4fec-8cae-4206ed93cba4":           true,
		`59145519-2c9d-4fec-8cae-4206ed93cba4" -port "1`: false,
		"59145519-2c9d-4fec-8cae-4206ed93cba4\n":         false,
		"":                                               false,
	} {
		if guidPattern.MatchString(s) != want {
			t.Errorf("guidPattern(%q) != %v", s, want)
		}
	}
	for s, want := range map[string]bool{
		"CA30544A4EE05749BC6D29B89B2CA65FC13232FD": true,
		"CA30544A4EE05749BC6D29B89B2CA65FC13232F":  false,
		"CA30544A4EE05749BC6D29B89B2CA65FC132 2FD": false,
		`CA30544A4EE05749BC6D29B89B2CA65FC1323"FD`: false,
	} {
		if isFingerprint(s) != want {
			t.Errorf("isFingerprint(%q) != %v", s, want)
		}
	}
}

// A file left empty is kept: it may be one the user created before install.
func TestRemoveBlockKeepsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpg.conf")
	if err := upsertBlock(path, "export X=1", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeBlock(path); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoveBlockMissingFile(t *testing.T) {
	if err := removeBlock(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFileReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "sub", "dst")
	writeFile(t, src, "v1", 0o600)
	if err := installFile(src, dst, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "v2", 0o600)
	if err := installFile(src, dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dst); got != "v2" {
		t.Fatalf("dst = %q", got)
	}
	mustNotExist(t, dst+".new")
	mustNotExist(t, dst+".old")
	// Installing a file onto itself (reinstall from the installed copy) is a no-op.
	if err := installFile(dst, dst, 0o700); err != nil {
		t.Fatal(err)
	}
}

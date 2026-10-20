//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Edits to the user's shell startup files. Only the lines between the wyk
// markers are ever written or removed; every other byte stays as it was.

// The markers around wyk's lines.
const (
	blockBegin = "# >>> wyk >>>"
	blockEnd   = "# <<< wyk <<<"
)

// shellBlock sets SSH_AUTH_SOCK to wyk's socket unless something else did.
const shellBlock = `if [ -z "${SSH_AUTH_SOCK:-}" ]; then
  export SSH_AUTH_SOCK="/run/user/$(id -u)/` + sshSock + `"
fi`

// upsertBlock writes body between wyk markers, replacing a previous block.
func upsertBlock(path, body string, mode os.FileMode) error {
	old, err := os.ReadFile(path) //nolint:gosec // G304: the user's own dotfile
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rest, err := stripBlock(old)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if len(rest) > 0 && !bytes.HasSuffix(rest, []byte("\n")) {
		rest = append(rest, '\n')
	}
	out := append(rest, []byte(blockBegin+"\n"+body+"\n"+blockEnd+"\n")...)
	return writeKeepingMode(path, out, mode)
}

func removeBlock(path string) error {
	old, err := os.ReadFile(path) //nolint:gosec // G304: the user's own dotfile
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stripped, err := stripBlock(old)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(stripped, old) {
		return nil
	}
	return writeKeepingMode(path, stripped, 0o644)
}

// stripBlock removes our marked block and leaves every other byte as is. It
// refuses a begin marker without an end marker rather than guess where the
// block stops and drop the user's lines.
func stripBlock(b []byte) ([]byte, error) {
	begin := []byte(blockBegin + "\n")
	end := []byte(blockEnd + "\n")
	s := append([]byte("\n"), b...) // so a block on the first line matches too
	if !bytes.HasSuffix(s, []byte("\n")) {
		s = append(s, '\n')
	}
	i := bytes.Index(s, append([]byte("\n"), begin...))
	if i < 0 {
		return b, nil
	}
	n := bytes.Index(s[i:], append([]byte("\n"), end...))
	if n < 0 {
		return nil, fmt.Errorf("%q without %q, fix the file by hand", blockBegin, blockEnd)
	}
	out := append(s[1:i+1:i+1], s[i+n+1+len(end):]...)
	if !bytes.HasSuffix(b, []byte("\n")) {
		out = bytes.TrimSuffix(out, []byte("\n"))
	}
	return out, nil
}

// writeKeepingMode replaces a file atomically, so a crash or a full disk
// leaves the old or the new content, never a truncated dotfile. It writes
// through symlinks (dotfile managers link ~/.bashrc elsewhere) and keeps the
// existing mode. The rename does break a hard link and needs a writable
// directory; both fail visibly, unlike a half-written ~/.bashrc.
func writeKeepingMode(path string, data []byte, mode os.FileMode) error {
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// A dangling link is an error here, not something to replace.
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return err
		}
	}
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := target + ".wyk-new"
	err := os.WriteFile(tmp, data, mode) //nolint:gosec // G703: next to the user's own dotfile
	if err == nil {
		err = os.Chmod(tmp, mode) // WriteFile applies the umask
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		return errors.Join(err, removeIfExists(tmp))
	}
	return nil
}

// shellFiles are the startup files that get the SSH_AUTH_SOCK block:
// ~/.profile and ~/.bashrc, and the bash login files that exist, since a
// login bash reads the first of those instead of ~/.profile. With all, it
// lists every one, for uninstall.
func shellFiles(home string, all bool) []string {
	files := []string{filepath.Join(home, ".profile"), filepath.Join(home, ".bashrc")}
	for _, name := range []string{".bash_profile", ".bash_login"} {
		f := filepath.Join(home, name)
		if _, err := os.Stat(f); all || err == nil {
			files = append(files, f)
		}
	}
	return files
}

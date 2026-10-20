//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cmd/wyk-host/relay.sh is what wyk-host.exe runs in the distro; it is
// tested here, where there is a shell, with a fake relay on stdin.

// fakeRelay answers "version" like the real one and otherwise records its
// arguments and $WYK_TEST_ENV, then exits with $WYK_TEST_EXIT.
const fakeRelay = `#!/bin/sh
if [ "$1" = version ]; then echo v-test; exit 0; fi
printf '%s\n' "$*" "${WYK_TEST_ENV:-}" >"$HOME/ran"
exit "${WYK_TEST_EXIT:-0}"
`

const profileSetsEnv = "export WYK_TEST_ENV=from-profile\n"

type scriptRun struct {
	home   string
	out    string
	status int
}

// runScript runs relay.sh as wyk-host.exe would: built for machine at
// version ver, in a fresh HOME with that ~/.profile and login shell.
func runScript(t *testing.T, profile, shell, machine, ver string, args ...string) scriptRun {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "wyk-host", "relay.sh"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{"-c", string(script), "sh", `C:\wyk.exe`, machine, ver}, args...)...) //nolint:gosec // G204: the script under test
	cmd.Env = []string{"HOME=" + home, "SHELL=" + shell, "PATH=" + os.Getenv("PATH")}
	if v, ok := os.LookupEnv("WYK_TEST_EXIT"); ok {
		cmd.Env = append(cmd.Env, "WYK_TEST_EXIT="+v)
	}
	cmd.Stdin = strings.NewReader(fakeRelay)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	r := scriptRun{home: home}
	if err := cmd.Run(); err != nil {
		exit := (*exec.ExitError)(nil)
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		r.status = exit.ExitCode()
	}
	r.out = out.String()
	return r
}

func thisMachine(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// ran returns what the fake relay recorded, or "" if it never ran a
// subcommand.
func (r scriptRun) ran(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.home, "ran"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noStaging checks that the staged relay is gone, wherever it was staged.
func (r scriptRun) noStaging(t *testing.T) {
	t.Helper()
	for _, dir := range []string{r.home, filepath.Join(r.home, ".local", "bin")} {
		if m, _ := filepath.Glob(filepath.Join(dir, ".wyk-install.*")); len(m) > 0 {
			t.Fatalf("staged relay left behind: %v", m)
		}
	}
}

func TestRelayScriptRunsSubcommandInLoginEnvironment(t *testing.T) {
	// sh, a login shell with -l, and one that is gone (sh stands in).
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/nonexistent/zsh"} {
		r := runScript(t, profileSetsEnv, shell, thisMachine(t), "v-test", "uninstall", "-flag", "two words")
		if r.status != 0 {
			t.Fatalf("SHELL=%s: status %d: %s", shell, r.status, r.out)
		}
		// The arguments arrive unchanged, and ~/.profile was read.
		if got, want := r.ran(t), "uninstall -flag two words\nfrom-profile\n"; got != want {
			t.Fatalf("SHELL=%s: relay saw %q, want %q", shell, got, want)
		}
		r.noStaging(t)
		// Uninstall stages in $HOME rather than create ~/.local/bin.
		if _, err := os.Stat(filepath.Join(r.home, ".local")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SHELL=%s: uninstall created ~/.local: %v", shell, err)
		}
	}
}

func TestRelayScriptKeepsRelayStatus(t *testing.T) {
	t.Setenv("WYK_TEST_EXIT", "7")
	// set -e in a profile must not swallow the relay's status either.
	for _, profile := range []string{"", "set -e\n"} {
		if r := runScript(t, profile, "/bin/sh", thisMachine(t), "v-test", "uninstall"); r.status != 7 {
			t.Fatalf("profile %q: status %d, want the relay's 7: %s", profile, r.status, r.out)
		}
	}
}

func TestRelayScriptRefusesOtherMachine(t *testing.T) {
	r := runScript(t, profileSetsEnv, "/bin/sh", "not-"+thisMachine(t), "v-test", "uninstall")
	if r.status == 0 || !strings.Contains(r.out, "this build is for") || r.ran(t) != "" {
		t.Fatalf("status %d, ran %q: %s", r.status, r.ran(t), r.out)
	}
}

func TestRelayScriptRefusesOtherVersion(t *testing.T) {
	r := runScript(t, profileSetsEnv, "/bin/sh", thisMachine(t), "v-other", "uninstall")
	if r.status == 0 || !strings.Contains(r.out, "embedded wyk-relay is v-test") || r.ran(t) != "" {
		t.Fatalf("status %d, ran %q: %s", r.status, r.ran(t), r.out)
	}
	r.noStaging(t)
}

// A profile that execs another program ends the login shell before the
// relay runs, with status 0: that must be an error, not a silent success.
func TestRelayScriptNoticesProfileExec(t *testing.T) {
	r := runScript(t, "exec true\n", "/bin/sh", thisMachine(t), "v-test", "uninstall")
	if r.status == 0 || !strings.Contains(r.out, "did not report back") || r.ran(t) != "" {
		t.Fatalf("status %d, ran %q: %s", r.status, r.ran(t), r.out)
	}
	r.noStaging(t)
}

// Install needs the exe where the distro can see it: wslpath fails, or
// finds nothing, for C:\wyk.exe here.
func TestRelayScriptInstallNeedsVisibleExe(t *testing.T) {
	r := runScript(t, profileSetsEnv, "/bin/sh", thisMachine(t), "v-test", "install")
	if r.status == 0 || !strings.Contains(r.out, "cannot see") || r.ran(t) != "" {
		t.Fatalf("status %d, ran %q: %s", r.status, r.ran(t), r.out)
	}
}

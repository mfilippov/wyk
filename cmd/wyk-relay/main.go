//go:build linux

// wyk-relay runs inside a WSL distro. It starts wyk-host.exe through interop,
// exposes unix sockets for ssh and gpg, and forwards each accepted
// connection to the host over vsock.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mfilippov/wyk/internal/bridge"
	"github.com/mfilippov/wyk/internal/proto"
	"github.com/mfilippov/wyk/internal/vsock"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

type relay struct {
	port  uint32
	token []byte
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("wyk-relay: ")
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			cmdInstall(os.Args[2:])
			return
		case "uninstall":
			cmdUninstall(os.Args[2:])
			return
		case "version":
			fmt.Println(version)
			return
		}
	}
	run(os.Args[1:])
}

func run(args []string) {
	fs := flag.NewFlagSet("wyk-relay", flag.ExitOnError)
	hostExe := fs.String("host", defaultHostExe(), "path to wyk-host.exe")
	// Without systemd socket activation (a manual run, for debugging) the
	// sockets to serve must be named explicitly.
	sshSock := fs.String("ssh", "", "unix socket to serve as SSH_AUTH_SOCK, when not socket-activated")
	gpgSock := fs.String("gpg", "", "unix socket to serve as the gpg-agent socket, when not socket-activated")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	log.Printf("version %s", version)
	syscall.Umask(0o077)

	// Sockets first, so a bad path fails before any Windows process starts.
	// systemd hands them over by name; a manual run creates them, and those
	// files are ours to remove.
	listeners, err := activationListeners()
	if err != nil {
		log.Fatal(err)
	}
	var socks []string
	cleanup := func() {
		for _, p := range socks {
			if err := os.Remove(p); err != nil {
				log.Print(err)
			}
		}
	}
	// Before dying, refuse the connections already queued: left there, they
	// make systemd start the relay again at once, a Windows process each time,
	// until the socket's trigger limit fails it for good. (The unit turns off
	// the service start limit; this is what keeps the trigger limit away.)
	fatal := func(v ...any) {
		for _, l := range listeners {
			refuseQueued(l)
		}
		cleanup()
		log.Fatal(v...)
	}
	if listeners == nil {
		listeners = map[string]*net.UnixListener{}
		for name, path := range map[string]string{"ssh": *sshSock, "gpg": *gpgSock} {
			if path == "" {
				continue
			}
			l, err := listenUnix(path)
			if err != nil {
				fatal(name, ": ", err)
			}
			socks = append(socks, path)
			listeners[name] = l
		}
	}
	if len(listeners) == 0 {
		fatal("nothing to serve: run as wyk-relay.service, or pass -ssh and/or -gpg")
	}
	channels := map[string]proto.Channel{"ssh": proto.ChanSSH, "gpg": proto.ChanGPG}
	for name := range listeners {
		if channels[name] == 0 {
			fatal("unexpected socket ", name)
		}
	}

	r := &relay{token: make([]byte, proto.TokenSize)}
	rand.Read(r.token) // crashes the program rather than return an error
	host, err := r.startHost(*hostExe)
	if err != nil {
		fatal(err)
	}
	for name, l := range listeners {
		log.Printf("%s: serving %s", channels[name], l.Addr())
		go r.serve(l, channels[name])
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	exited := make(chan error, 1)
	go func() { exited <- host.Wait() }()
	select {
	case s := <-sig:
		log.Printf("%s, shutting down", s)
		cleanup()
	case err := <-exited:
		fatal("wyk-host exited: ", err)
	}
}

// refuseQueued closes every connection already waiting on l; each client
// sees its connection end.
func refuseQueued(l *net.UnixListener) {
	// Slightly ahead: with a deadline already past, Accept does not even try.
	if l.SetDeadline(time.Now().Add(50*time.Millisecond)) != nil {
		return
	}
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return // the deadline: nothing more is queued
		}
		_ = c.Close() // a client hanging up first is no news
	}
}

var guidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}$`)

// Waits for the Windows side: process start through interop, and a handshake
// that may first have to launch gpg-agent.
const (
	hostStartTimeout = 60 * time.Second
	handshakeTimeout = 15 * time.Second
)

// startHost launches wyk-host.exe on a random port and waits for "ready".
// A port clash with another distro's host makes it exit; we then retry.
func (r *relay) startHost(exe string) (*exec.Cmd, error) {
	out, err := output("wslinfo", "--vm-id", "-n")
	if err != nil {
		return nil, fmt.Errorf("%w (needs WSL 2.6.1 or later)", err)
	}
	vmid := strings.TrimSpace(string(out))
	// Interop flattens argv into a Windows command line that wyk-host.exe
	// splits again, so only pass a value that cannot contain separators.
	if !guidPattern.MatchString(vmid) {
		return nil, fmt.Errorf("wslinfo --vm-id: unexpected %q", vmid)
	}
	for attempt := 0; attempt < 3; attempt++ {
		r.port = randomPort()
		cmd, err := r.tryHost(exe, vmid)
		if err == nil {
			return cmd, nil
		}
		// A host that never answered would only make the client wait again.
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, err
		}
		log.Printf("wyk-host did not start on port %d: %v", r.port, err)
	}
	return nil, errors.New("wyk-host failed to start")
}

// tryHost starts one wyk-host.exe. The token, the only secret between relay
// and host, goes over stdin with the port: anyone in the distro can read a
// process's command line.
func (r *relay) tryHost(exe, vmid string) (*exec.Cmd, error) {
	ready, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// A read end has nothing to report on close.
	defer func() { _ = ready.Close() }()
	cmd := exec.Command(exe, "serve", "-vmid", vmid) //nolint:gosec // G204: our own host exe; vmid checked by the caller
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	// stdin stays open for the host's lifetime; closing it stops the host.
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start() // on failure Start closes the stdin pipe itself
	}
	if err != nil {
		return nil, errors.Join(err, w.Close())
	}
	// The host holds its own copy of w.
	if err := errors.Join(w.Close(), r.handOver(stdin, ready)); err != nil {
		_ = cmd.Process.Kill() // fails only if the host already exited; Wait says how
		return nil, errors.Join(err, cmd.Wait())
	}
	return cmd, nil
}

// handOver sends the host its token and port and waits for its "ready".
func (r *relay) handOver(stdin io.Writer, ready *os.File) error {
	if _, err := fmt.Fprintf(stdin, "%x %d\n", r.token, r.port); err != nil {
		return err
	}
	if err := ready.SetReadDeadline(time.Now().Add(hostStartTimeout)); err != nil {
		return err
	}
	line, err := bufio.NewReader(ready).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "ready" {
		return fmt.Errorf("unexpected %q", line)
	}
	return nil
}

func randomPort() uint32 {
	var b [4]byte
	rand.Read(b[:]) // crashes the program rather than return an error
	// Stay clear of the low range WSL itself uses.
	return 0x40000000 | binary.LittleEndian.Uint32(b[:])&0x3fffffff
}

// listenUnix replaces a stale socket file but refuses to steal a live one,
// or to remove anything that is not a socket (a mistyped -ssh path).
func listenUnix(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.Dial("unix", path); err == nil {
		return nil, errors.Join(fmt.Errorf("%s is already served by another process", path), c.Close())
	}
	if err := removeIfExists(path); err != nil {
		return nil, err
	}
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}

// serve accepts until the listener is closed. Other accept errors (EMFILE
// and the like) pass, and giving up on them would leave the socket dead.
func (r *relay) serve(l *net.UnixListener, ch proto.Channel) {
	for {
		c, err := l.AcceptUnix()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Printf("%s: accept: %v", ch, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go r.forward(c, ch)
	}
}

func (r *relay) forward(c *net.UnixConn, ch proto.Channel) {
	v, err := r.dialHost(ch)
	if err != nil {
		log.Printf("%s: %v", ch, errors.Join(err, c.Close()))
		return
	}
	bridge.Join(c, v)
}

// dialHost opens a vsock connection to the host and completes the handshake.
func (r *relay) dialHost(ch proto.Channel) (*vsock.Conn, error) {
	v, err := vsock.Dial(vsock.CIDHost, r.port, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := r.handshake(v, ch); err != nil {
		return nil, errors.Join(err, v.Close())
	}
	return v, nil
}

func (r *relay) handshake(v *vsock.Conn, ch proto.Channel) error {
	if err := v.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	if err := proto.WriteHello(v, ch, r.token); err != nil {
		return err
	}
	if err := proto.ReadStatus(v); err != nil {
		return err
	}
	return v.SetDeadline(time.Time{})
}

func defaultHostExe() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "wyk-host.exe")
	}
	return "wyk-host.exe"
}

// output runs a command, no shell involved, and returns its stdout.
func output(name string, args ...string) ([]byte, error) {
	return runCmd(exec.Command(name, args...)) //nolint:gosec // G204: callers run their own commands with checked arguments
}

// runCmd runs cmd and returns its stdout. An error names the command and
// carries what it said on stderr.
func runCmd(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exit.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return out, nil
}

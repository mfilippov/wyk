//go:build windows

// wyk-host is the Windows half of wyk, and the exe users download. Run by
// hand, it installs wyk-relay, embedded in it, into a WSL distro (see
// install.go). Started by wyk-relay through WSL interop ("serve"), it reads a
// token from stdin, listens on the WSL VM's Hyper-V socket, prints "ready"
// and serves until stdin is closed.
package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mfilippov/wyk/internal/bridge"
	"github.com/mfilippov/wyk/internal/hvsock"
	"github.com/mfilippov/wyk/internal/proto"
)

// Anything in the VM can connect before proving it knows the token. Each
// pending handshake pins an OS thread, so bound both their time and number.
const (
	handshakeTimeout = 2 * time.Second
	maxHandshakes    = 32
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

type server struct {
	token      []byte
	handshakes chan struct{}
	// dial connects to the Windows agent behind a channel; dialAgent, but
	// tests put a fake agent there.
	dial func(proto.Channel) (bridge.Conn, error)
}

func newServer(token []byte) *server {
	return &server{token: token, handshakes: make(chan struct{}, maxHandshakes), dial: dialAgent}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("wyk-host: ")
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	switch sub {
	case "serve": // what wyk-relay runs
		serve(os.Args[2:])
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "localappdata": // for wyk-relay install, which puts a copy of this exe there
		local, err := os.UserCacheDir() // %LocalAppData% on Windows
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(local)
	case "help", "-h", "-help", "--help":
		usage(0)
	default: // nothing (a double click) or a typo
		if sub != "" {
			log.Printf("unknown command %q", sub) //nolint:gosec // G706: %q quotes it
		}
		usage(2)
	}
}

// serve is the side the VM sees: it takes the token and port from stdin,
// listens on the VM's Hyper-V socket and serves until stdin is closed.
func serve(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	vmid := flags.String("vmid", "", "WSL VM id (wslinfo --vm-id)")
	if err := flags.Parse(args); err != nil {
		log.Fatal(err)
	}

	// Started from WSL, the working directory is \wsl.localhost\...; a
	// gpg-agent started from here would keep it for life. The profile folder
	// is one that wyk never removes, unlike the folder of this exe. Failing
	// that, forwarding still works; only uninstall may then trip over it.
	home, err := os.UserHomeDir()
	if err == nil {
		err = os.Chdir(home)
	}
	if err != nil {
		log.Printf("staying in the inherited working directory: %v", err)
	}

	vm, err := hvsock.ParseGUID(*vmid)
	if err != nil {
		log.Fatal(err)
	}

	// The relay writes "<token hex> <port>" on stdin, never on the command
	// line, which anyone in the distro can read.
	stdin := bufio.NewReader(os.Stdin)
	line, err := stdin.ReadString('\n')
	if err != nil {
		log.Fatalf("read stdin: %v", err)
	}
	tokenHex, portStr, _ := strings.Cut(strings.TrimSpace(line), " ")
	token, err := hex.DecodeString(tokenHex)
	if err != nil || len(token) != proto.TokenSize {
		log.Fatal("bad token on stdin")
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil || port == 0 {
		log.Fatal("bad port on stdin")
	}

	// An upgrade renames the running exe aside; the old process is gone now.
	if exe, err := os.Executable(); err == nil {
		if err := os.Remove(exe + ".old"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Print(err)
		}
	}

	l, err := hvsock.Listen(vm, hvsock.VsockServiceID(uint32(port)))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%s listening on %s", version, l.Addr())
	fmt.Println("ready")

	// The relay holds our stdin; EOF means it (or the whole VM) is gone.
	go func() {
		if _, err := io.Copy(io.Discard, stdin); err != nil {
			log.Fatalf("stdin: %v", err)
		}
		os.Exit(0)
	}()

	s := newServer(token)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Print(err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.admit(c)
	}
}

// admit gives c a handshake slot, or drops it when all are taken.
func (s *server) admit(c bridge.Conn) {
	select {
	case s.handshakes <- struct{}{}:
		go s.handle(c)
	default:
		log.Print("too many pending handshakes, dropping a connection")
		if err := c.Close(); err != nil {
			log.Print(err)
		}
	}
}

func (s *server) handle(c bridge.Conn) {
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()
	agent, err := s.open(c)
	if err != nil {
		log.Print(err)
		if status := proto.Status(0); errors.As(err, &status) {
			if err := proto.WriteStatus(c, status); err != nil {
				log.Printf("reply %q: %v", status, err)
			}
		}
		return
	}
	bridge.Join(c, agent)
}

// open runs the handshake and connects to the agent behind the channel.
// Errors the client should be told about wrap a proto.Status.
func (s *server) open(c bridge.Conn) (bridge.Conn, error) {
	ch, err := s.handshake(c)
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	agent, err := s.dial(ch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", ch, proto.StatusBackend, err)
	}
	if err := proto.WriteStatus(c, proto.StatusOK); err != nil {
		return nil, errors.Join(fmt.Errorf("%s: reply: %w", ch, err), agent.Close())
	}
	return agent, nil
}

// handshake reads the hello within handshakeTimeout, then frees its slot.
func (s *server) handshake(c bridge.Conn) (proto.Channel, error) {
	defer func() { <-s.handshakes }()
	// Close keeps its error; handle reports it from its own Close.
	timer := time.AfterFunc(handshakeTimeout, func() { _ = c.Close() })
	ch, token, err := proto.ReadHello(c)
	if !timer.Stop() {
		return 0, errors.New("timed out")
	}
	switch {
	case err != nil:
		return 0, err
	case subtle.ConstantTimeCompare(token, s.token) != 1:
		return 0, proto.StatusBadToken
	case ch != proto.ChanSSH && ch != proto.ChanGPG:
		return 0, fmt.Errorf("%s: %w", ch, proto.StatusBadChannel)
	}
	return ch, nil
}

// dialAgent connects to the Windows agent behind a channel. Every backend
// is a plain duplex stream, so all channels share one bridge. gpg-agent
// creates both the pipe and the sockets, so start it if nothing answers.
func dialAgent(ch proto.Channel) (bridge.Conn, error) {
	dial := func() (bridge.Conn, error) {
		if ch == proto.ChanSSH {
			return dialPipe(sshAgentPipe)
		}
		sock, err := extraSocket()
		if err != nil {
			return nil, err
		}
		return dialAssuan(sock)
	}
	c, err := dial()
	if err == nil {
		return c, nil
	}
	log.Printf("%s: %v; starting gpg-agent and retrying", ch, err)
	if _, err := gpgconf("--launch", "gpg-agent"); err != nil {
		log.Print(err)
	}
	return dial()
}

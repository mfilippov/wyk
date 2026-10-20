//go:build linux

package main

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A mistyped -ssh path must not delete the file it points at.
func TestListenUnixKeepsRegularFiles(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id_ed25519")
	writeFile(t, key, "secret", 0o600)
	if l, err := listenUnix(key); err == nil {
		t.Fatalf("listened on a regular file (close: %v)", l.Close())
	}
	if got := readFile(t, key); got != "secret" {
		t.Fatalf("file changed to %q", got)
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false) // leave a stale socket file behind
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = listenUnix(path)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// A relay that fails must take the queued connections off the socket, or
// systemd starts it again for the same ones until it gives up on the socket.
func TestRefuseQueuedDrainsTheQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	refuseQueued(l)
	for _, c := range clients {
		if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("queued client not refused: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}
}

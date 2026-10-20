//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mfilippov/wyk/internal/bridge"
	"github.com/mfilippov/wyk/internal/proto"
)

// pipeEnd is one end of a net.Pipe as a bridge.Conn. A pipe has no
// half-close, so CloseWrite closes it, as pipeConn does for named pipes.
type pipeEnd struct{ net.Conn }

func (p pipeEnd) CloseWrite() error { return p.Close() }

var testToken = bytes.Repeat([]byte{7}, proto.TokenSize)

// testServer serves one connection, as the accept loop would, with a fake
// agent that echoes. It returns the client's end and how often the server
// dialed the agent.
func testServer(t *testing.T) (*server, *atomic.Int32) {
	t.Helper()
	s := newServer(testToken)
	var dials atomic.Int32
	s.dial = func(proto.Channel) (bridge.Conn, error) {
		dials.Add(1)
		agent, far := net.Pipe()
		go func() { _, _ = io.Copy(far, far); _ = far.Close() }() // echo
		return pipeEnd{agent}, nil
	}
	return s, &dials
}

// connect hands a new connection to s and returns the client's end.
func connect(t *testing.T, s *server) net.Conn {
	t.Helper()
	client, conn := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	s.admit(pipeEnd{conn})
	return client
}

func hello(t *testing.T, c net.Conn, ch proto.Channel, token []byte) error {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteHello(c, ch, token); err != nil {
		t.Fatal(err)
	}
	return proto.ReadStatus(c)
}

func TestHandshakeWithTokenReachesAgent(t *testing.T) {
	for _, ch := range []proto.Channel{proto.ChanSSH, proto.ChanGPG} {
		s, dials := testServer(t)
		c := connect(t, s)
		if err := hello(t, c, ch, testToken); err != nil {
			t.Fatalf("%s: %v", ch, err)
		}
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 4)
		if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
			t.Fatalf("%s: echo %q, %v", ch, got, err)
		}
		if dials.Load() != 1 {
			t.Fatalf("%s: %d dials", ch, dials.Load())
		}
	}
}

// Nothing but the right token gets near the agent.
func TestHandshakeRejectsBadToken(t *testing.T) {
	s, dials := testServer(t)
	wrong := bytes.Repeat([]byte{8}, proto.TokenSize)
	if err := hello(t, connect(t, s), proto.ChanSSH, wrong); !errors.Is(err, proto.StatusBadToken) {
		t.Fatalf("got %v, want %v", err, proto.StatusBadToken)
	}
	if dials.Load() != 0 {
		t.Fatal("dialed the agent for a bad token")
	}
}

func TestHandshakeRejectsUnknownChannel(t *testing.T) {
	s, dials := testServer(t)
	for _, ch := range []proto.Channel{0, 3, 255} {
		if err := hello(t, connect(t, s), ch, testToken); !errors.Is(err, proto.StatusBadChannel) {
			t.Fatalf("channel %d: got %v, want %v", ch, err, proto.StatusBadChannel)
		}
	}
	if dials.Load() != 0 {
		t.Fatal("dialed the agent for an unknown channel")
	}
}

func TestHandshakeRejectsBadMagic(t *testing.T) {
	s, dials := testServer(t)
	c := connect(t, s)
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	b := append([]byte("WYK2\x01"), testToken...)
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	// No status for a peer that does not speak the protocol; it is closed.
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("got a reply to a bad magic")
	}
	if dials.Load() != 0 {
		t.Fatal("dialed the agent for a bad magic")
	}
}

// A silent client is cut off after handshakeTimeout and frees its slot.
func TestHandshakeTimesOut(t *testing.T) {
	s, _ := testServer(t)
	c := connect(t, s)
	start := time.Now()
	if err := c.SetDeadline(start.Add(handshakeTimeout + 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent client got data")
	}
	if took := time.Since(start); took < handshakeTimeout || took > handshakeTimeout+3*time.Second {
		t.Fatalf("closed after %v, want about %v", took, handshakeTimeout)
	}
	waitFor(t, func() bool { return len(s.handshakes) == 0 })
}

// Past maxHandshakes pending handshakes, new connections are dropped at
// once; finished handshakes free their slots.
func TestHandshakeSlotsAreBounded(t *testing.T) {
	s, _ := testServer(t)
	for range maxHandshakes {
		connect(t, s) // silent: holds its slot until the timeout
	}
	// admit has closed it before returning, so it reads EOF without waiting.
	start := time.Now()
	if _, err := connect(t, s).Read(make([]byte, 1)); !errors.Is(err, io.EOF) || time.Since(start) > handshakeTimeout/4 {
		t.Fatalf("connection past the limit: %v after %v, want it closed at once", err, time.Since(start))
	}
	waitFor(t, func() bool { return len(s.handshakes) == 0 }) // the timeouts free them
	if err := hello(t, connect(t, s), proto.ChanSSH, testToken); err != nil {
		t.Fatalf("after the slots were freed: %v", err)
	}
}

func TestBackendFailureIsReported(t *testing.T) {
	s, _ := testServer(t)
	s.dial = func(proto.Channel) (bridge.Conn, error) { return nil, errors.New("no agent") }
	if err := hello(t, connect(t, s), proto.ChanGPG, testToken); !errors.Is(err, proto.StatusBackend) {
		t.Fatalf("got %v, want %v", err, proto.StatusBackend)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(handshakeTimeout + 5*time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

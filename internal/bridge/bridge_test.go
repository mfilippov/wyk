package bridge

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(t, err)
	defer func() { check(t, l.Close()) }()
	type result struct {
		c   net.Conn
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		c, err := l.Accept()
		accepted <- result{c, err}
	}()
	dialed, err := net.Dial("tcp", l.Addr().String())
	check(t, err)
	r := <-accepted
	check(t, r.err)
	return dialed.(*net.TCPConn), r.c.(*net.TCPConn)
}

// joined connects client <-> [a  Join  b] <-> server and returns a channel
// closed when Join returns.
func joined(t *testing.T) (client, server *net.TCPConn, done chan struct{}) {
	t.Helper()
	client, a := tcpPair(t)
	b, server := tcpPair(t)
	t.Cleanup(func() {
		for _, c := range []*net.TCPConn{client, server} {
			if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		}
	})
	done = make(chan struct{})
	go func() {
		Join(a, b)
		close(done)
	}()
	return client, server, done
}

func waitJoin(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Join did not return")
	}
}

func TestJoinHalfClose(t *testing.T) {
	client, server, done := joined(t)

	// The client finishes sending first; the server must see EOF and
	// still be able to answer.
	_, err := io.WriteString(client, "request")
	check(t, err)
	check(t, client.CloseWrite())
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "request" {
		t.Fatalf("server read %q, %v", got, err)
	}
	_, err = io.WriteString(server, "response")
	check(t, err)
	check(t, server.CloseWrite())
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "response" {
		t.Fatalf("client read %q, %v", got, err)
	}
	waitJoin(t, done)
}

// A hard failure on one side must not leave the other copy hanging.
func TestJoinAbort(t *testing.T) {
	client, server, done := joined(t)
	check(t, server.SetLinger(0))
	check(t, server.Close()) // RST towards Join
	_, err := io.WriteString(client, "x")
	check(t, err)
	waitJoin(t, done)
}

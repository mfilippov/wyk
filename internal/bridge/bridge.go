// Package bridge copies bytes between two full-duplex streams.
package bridge

import (
	"io"
	"sync"
)

// Conn is a stream that can signal end of output without closing input.
type Conn interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// Join copies a<->b until both directions finish, propagating half-close,
// then closes both. A failure in either direction closes both at once so
// the other copy cannot hang.
func Join(a, b Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			// Teardown: the session is over either way, and a stream may
			// already be closed (a pipe's CloseWrite closes it).
			_ = a.Close()
			_ = b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pump(b, a, closeBoth)
	}()
	pump(a, b, closeBoth)
	wg.Wait()
	closeBoth()
}

func pump(dst, src Conn, abort func()) {
	if _, err := io.Copy(dst, src); err != nil || dst.CloseWrite() != nil {
		abort()
	}
}

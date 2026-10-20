package proto

import (
	"bytes"
	"errors"
	"testing"
)

func TestHelloRoundTrip(t *testing.T) {
	token := bytes.Repeat([]byte{0xab}, TokenSize)
	var buf bytes.Buffer
	if err := WriteHello(&buf, ChanGPG, token); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != helloSize {
		t.Fatalf("hello is %d bytes, want %d", buf.Len(), helloSize)
	}
	ch, got, err := ReadHello(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if ch != ChanGPG || !bytes.Equal(got, token) {
		t.Fatalf("got %s %x", ch, got)
	}
}

func TestWriteHelloRejectsShortToken(t *testing.T) {
	if err := WriteHello(&bytes.Buffer{}, ChanSSH, []byte{1, 2, 3}); err == nil {
		t.Fatal("want error")
	}
}

func TestReadHelloBadMagic(t *testing.T) {
	b := make([]byte, helloSize)
	copy(b, "HTTP")
	if _, _, err := ReadHello(bytes.NewReader(b)); !errors.Is(err, errBadMagic) {
		t.Fatalf("err = %v, want bad magic", err)
	}
}

func TestReadHelloShort(t *testing.T) {
	if _, _, err := ReadHello(bytes.NewReader([]byte("WYK1"))); err == nil {
		t.Fatal("want error on truncated hello")
	}
}

func TestStatus(t *testing.T) {
	for _, s := range []Status{StatusOK, StatusBadToken, StatusBadChannel, StatusBackend} {
		var buf bytes.Buffer
		if err := WriteStatus(&buf, s); err != nil {
			t.Fatal(err)
		}
		err := ReadStatus(&buf)
		if s == StatusOK {
			if err != nil {
				t.Fatalf("OK: %v", err)
			}
			continue
		}
		var got Status
		if !errors.As(err, &got) || got != s {
			t.Fatalf("status %d: err = %v", s, err)
		}
	}
}

// Package proto defines the handshake that opens every vsock connection
// between wyk-relay and wyk-host. After the handshake the connection carries
// the raw agent protocol of the selected channel.
//
//	relay -> host: "WYK1" | channel u8 | token [32]byte
//	host -> relay: status u8
package proto

import (
	"errors"
	"fmt"
	"io"
)

const (
	magic     = "WYK1"
	TokenSize = 32
	helloSize = len(magic) + 1 + TokenSize
)

// Channel selects the Windows-side agent socket.
type Channel byte

const (
	ChanSSH Channel = 1 // ssh-agent protocol (OpenSSH named pipe)
	ChanGPG Channel = 2 // gpg-agent's extra socket: sign and decrypt, no key or card management
)

func (c Channel) String() string {
	switch c {
	case ChanSSH:
		return "ssh"
	case ChanGPG:
		return "gpg"
	}
	return fmt.Sprintf("channel(%d)", byte(c))
}

// Status is the host's answer to the handshake.
type Status byte

const (
	StatusOK         Status = 0
	StatusBadToken   Status = 1
	StatusBadChannel Status = 2
	StatusBackend    Status = 3 // the Windows agent is unreachable
)

func (s Status) Error() string {
	switch s {
	case StatusBadToken:
		return "host rejected token"
	case StatusBadChannel:
		return "host does not serve this channel"
	case StatusBackend:
		return "host cannot reach the Windows agent"
	}
	return fmt.Sprintf("status %d", byte(s))
}

var errBadMagic = errors.New("bad handshake magic")

func WriteHello(w io.Writer, ch Channel, token []byte) error {
	if len(token) != TokenSize {
		return fmt.Errorf("token must be %d bytes", TokenSize)
	}
	b := make([]byte, 0, helloSize)
	b = append(b, magic...)
	b = append(b, byte(ch))
	b = append(b, token...)
	_, err := w.Write(b)
	return err
}

func ReadHello(r io.Reader) (Channel, []byte, error) {
	b := make([]byte, helloSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	if string(b[:len(magic)]) != magic {
		return 0, nil, errBadMagic
	}
	return Channel(b[len(magic)]), b[len(magic)+1:], nil
}

func WriteStatus(w io.Writer, s Status) error {
	_, err := w.Write([]byte{byte(s)})
	return err
}

// ReadStatus returns nil for StatusOK and the Status itself otherwise.
func ReadStatus(r io.Reader) error {
	b := make([]byte, 1)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	if s := Status(b[0]); s != StatusOK {
		return s
	}
	return nil
}

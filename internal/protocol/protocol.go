// Package protocol implements bounded, private guest/runner framing. Never log
// a Message: Delivery carries the supplied bearer credential only in memory.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"

	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/sys/unix"
)

var ErrProtocol = errors.New("private channel: invalid frame")

const MaxFrame = 1 << 20
const Port = 1024

type Delivery struct {
	LaunchID string         `json:"launch_id"`
	Identity string         `json:"identity"`
	Job      api.JobRef     `json:"job"`
	Lease    []byte         `json:"lease"`
	Facts    api.Allocation `json:"facts"`
	Platform string         `json:"platform"`
}
type Message struct {
	Kind     string    `json:"kind"`
	Delivery *Delivery `json:"delivery,omitempty"`
	Data     []byte    `json:"data,omitempty"`
}

func Write(w io.Writer, m Message) error {
	data, e := json.Marshal(m)
	if e != nil || len(data) > MaxFrame {
		return ErrProtocol
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if e = writeAll(w, header[:]); e != nil {
		return ErrProtocol
	}
	if e = writeAll(w, data); e != nil {
		return ErrProtocol
	}
	return nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func Read(r io.Reader) (Message, error) {
	var m Message
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return m, ErrProtocol
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxFrame {
		return m, ErrProtocol
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return m, ErrProtocol
	}
	if json.Unmarshal(b, &m) != nil {
		return Message{}, ErrProtocol
	}
	return m, nil
}
func SameUser(c *net.UnixConn) error {
	raw, e := c.SyscallConn()
	if e != nil {
		return ErrProtocol
	}
	good := false
	if raw.Control(func(fd uintptr) {
		u, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		good = e == nil && u.Uid == uint32(os.Geteuid())
	}) != nil || !good {
		return ErrProtocol
	}
	return nil
}

//go:build livefault

package runner

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

// This test executable uses the normal runner, its real handoff, verified VMM,
// and normal vsock dialing. Drop only an authenticated guest's first start
// acknowledgment on the legitimate client. Assignment writes are unchanged.
type lostGuestAck struct {
	net.Conn
	dir, identity string
}

func (c *lostGuestAck) Read(_ []byte) (int, error) {
	m, err := protocol.Read(c.Conn)
	if err != nil || m.Kind != "worker-started" || m.Delivery != nil || len(m.Data) != 0 {
		return 0, protocol.ErrProtocol
	}
	if state.Atomic(filepath.Join(c.dir, "ack-lost.json"), c.identity, nil) != nil {
		return 0, protocol.ErrProtocol
	}
	return 0, io.ErrUnexpectedEOF
}

func TestMain(m *testing.M) {
	if len(os.Args) < 2 || os.Args[1] != "runner" {
		os.Exit(m.Run())
	}
	if len(os.Args) != 6 || os.Args[2] != "--dir" || os.Args[4] != "--digest" {
		os.Exit(2)
	}
	dir := os.Args[3]
	spec, err := ReadSpec(dir)
	if err != nil || spec.LaunchID != "guest-lost-ack" || spec.TimeoutSeconds != 0 || Digest(spec) != os.Args[5] {
		os.Exit(2)
	}
	dial := func(path string, done <-chan struct{}) (net.Conn, error) {
		c, err := connectGuest(path, done)
		if err != nil {
			return nil, err
		}
		return &lostGuestAck{c, dir, spec.Identity}, nil
	}
	if err := execute(dir, os.Args[5], dial); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestLostAcknowledgementBoundary(t *testing.T) {
	for _, kind := range []string{"worker-started", "exit"} {
		t.Run(kind, func(t *testing.T) {
			d := t.TempDir()
			host, guest := net.Pipe()
			defer host.Close()
			go func() { defer guest.Close(); _ = protocol.Write(guest, protocol.Message{Kind: kind}) }()
			c := lostGuestAck{host, d, "identity"}
			if n, err := c.Read(make([]byte, 4)); n != 0 || err == nil {
				t.Fatal("guest acknowledgment reached the runner")
			}
			var received string
			err := state.Read(filepath.Join(d, "ack-lost.json"), &received)
			if kind == "worker-started" {
				if err != nil || received != "identity" {
					t.Fatal("actual acknowledgement loss unrecorded")
				}
			} else if err == nil {
				t.Fatal("wrong frame credited as a start")
			}
		})
	}
}

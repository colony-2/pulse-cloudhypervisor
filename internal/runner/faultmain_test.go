//go:build livefault

package runner

import (
	"net"
	"os"
	"testing"
)

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

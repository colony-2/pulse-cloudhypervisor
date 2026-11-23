package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestFramePreservesOpaqueLease(t *testing.T) {
	var b bytes.Buffer
	want := api.Lease("fixture\x00\xff\n ")
	m := Message{Kind: "deliver", Delivery: &Delivery{LaunchID: "launch", Identity: "identity", Lease: []byte(want)}}
	if Write(&b, m) != nil {
		t.Fatal("encode")
	}
	got, e := Read(&b)
	if e != nil || string(got.Delivery.Lease) != string(want) {
		t.Fatal("lease bytes changed")
	}
}
func TestPartialOversizedAndMalformedFrames(t *testing.T) {
	for _, raw := range [][]byte{nil, {0, 0}, {0, 0, 0, 5, '{'}, {0, 0, 0, 1, 'x'}} {
		if _, e := Read(bytes.NewReader(raw)); e == nil {
			t.Fatal("partial frame accepted")
		}
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], MaxFrame+1)
	if _, e := Read(bytes.NewReader(h[:])); e == nil {
		t.Fatal("oversized accepted")
	}
	if Write(io.Discard, Message{Data: make([]byte, MaxFrame)}) == nil {
		t.Fatal("oversized write")
	}
}
func TestSameUserUnixConnection(t *testing.T) {
	d, e := os.MkdirTemp("/tmp", "channel-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(d)
	path := filepath.Join(d, "s")
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, e := l.AcceptUnix()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		done <- SameUser(c)
	}()
	c, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if SameUser(c) != nil || <-done != nil {
		t.Fatal("peer auth failed")
	}
}

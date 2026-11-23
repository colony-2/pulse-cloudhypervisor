//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/sys/unix"
)

// Inspect one stream, including matches split over two reads. Drop all bytes
// after the scan. Never include a credential, unit property or process data in
// a test diagnostic.
func credentialPresent(r io.Reader, credential []byte) (bool, error) {
	buffer := make([]byte, 128*1024+len(credential))
	defer clear(buffer)
	carry := 0
	for {
		n, err := r.Read(buffer[carry:])
		block := buffer[:carry+n]
		if bytes.Contains(block, credential) {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		carry = copy(buffer, block[max(0, len(block)-len(credential)+1):])
	}
}

func TestS14(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "credential-lifetime", "valid", "lifetime")
	var wire struct {
		Version int
		Token   string `json:"leaseToken"`
	}
	if json.Unmarshal([]byte(l.Lease), &wire) != nil || wire.Version != 2 || len(wire.Token) < 12 || len(wire.Token) > 4096 {
		t.Fatal("disposable fixture token unavailable")
	}
	token := []byte(wire.Token)
	// Listen from dispatch. Check each source across its own chunk boundaries.
	out := new(guestOutput)
	opts := out.listen(e.ctx, l.LaunchID)
	observe := opts.Listener.OnLog
	tails := map[api.LogSource][]byte{}
	var exposed atomic.Bool
	opts.Listener.OnLog = func(ctx context.Context, event api.LaunchLog) error {
		// A subscription serializes its callbacks. Wait synchronizes final readback.
		data := append(tails[event.Source], event.Data...)
		if bytes.Contains(data, token) {
			exposed.Store(true)
		}
		tails[event.Source] = append([]byte(nil), data[max(0, len(data)-len(token)+1):]...)
		return observe(ctx, event)
	}
	sub := e.submit(t, l, opts)
	e.waitForWorker(t, out)
	dir := launchDirectory(e.config.StateDir, l.LaunchID)
	spec, err := runner.ReadSpec(dir)
	if err != nil || spec.Job != l.Job || spec.LaunchID != l.LaunchID {
		t.Fatal("owned launch unavailable")
	}
	main, vm := e.ownedPair(t, dir, spec)
	checkFile := func(root int, path string) error {
		fd, err := unix.Openat(root, path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), "private-observation")
		defer f.Close()
		exists, err := credentialPresent(f, token)
		if exists || err != nil {
			return errFixture
		}
		return nil
	}
	for _, process := range []ownedProcess{main, vm} {
		proc, err := unix.Open(filepath.Join("/proc", strconv.Itoa(process.pid)), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal("open owned process directory")
		}
		defer unix.Close(proc)
		wait := []unix.PollFd{{Fd: int32(process.fd), Events: unix.POLLIN}}
		if n, err := unix.Poll(wait, 0); err != nil || n != 0 {
			t.Fatal("owned process already exited")
		}
		for _, name := range []string{"cmdline", "environ"} {
			if checkFile(proc, name) != nil {
				t.Fatal("process credential inspection failed")
			}
		}
		if n, err := unix.Poll(wait, 0); err != nil || n != 0 {
			t.Fatal("process identity expired during credential inspection")
		}
	}
	properties, err := exec.CommandContext(e.ctx, e.config.Systemctl, "--user", "show", spec.Unit).Output()
	if err != nil || !bytes.Contains(properties, []byte("Description=pulse:"+spec.Identity+"\n")) || bytes.Contains(properties, token) {
		t.Fatal("unit credential inspection failed")
	}
	clear(properties)
	page, err := e.provider.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
	listing, marshalErr := json.Marshal(page)
	if err != nil || marshalErr != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job || bytes.Contains(listing, token) {
		t.Fatal("active listing credential inspection failed")
	}
	checkState := func() {
		t.Helper()
		err := filepath.WalkDir(e.config.StateDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				return checkFile(unix.AT_FDCWD, path)
			}
			if entry.IsDir() || entry.Type()&os.ModeSocket != 0 {
				return nil
			}
			return errFixture
		})
		if err != nil {
			t.Fatal("private provider state credential inspection failed")
		}
	}
	checkState()
	final := e.finish(t, l, sub, out, true)
	checkState()
	if exposed.Load() || !final.OutputOK || final.Outcome != "success" {
		t.Fatal("credential or authoritative workload observation failed")
	}
}

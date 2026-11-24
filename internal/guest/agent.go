// Package guest consumes exactly one supplied assignment inside a fresh guest.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

var ErrAgent = errors.New("guest: assignment not executed")

type Config struct {
	LaunchID, Identity, IntentDir, C2J string
	WorkDir                            string
	UID, GID                           uint32
	NestedKVM                          bool
}

// c2j requires a scaled SI or IEC unit even for a single byte. Decimal
// kilobytes can represent an unaligned byte count exactly without rounding.
func byteQuantity(n int64) string {
	if n%1024 == 0 {
		return strconv.FormatInt(n/1024, 10) + "Ki"
	}
	return fmt.Sprintf("%d.%03dk", n/1000, n%1000)
}

func Args(d protocol.Delivery) []string {
	args := []string{"run", "with-lease", "--ci", "--lease-file", "-", "--jobdb", d.Job.JobDB, "--job-id", d.Job.JobID, "--execution-base", d.Facts.Base, "--execution-platform", d.Platform, "--execution-cpu", strconv.FormatInt(d.Facts.CPUMillis, 10) + "m", "--execution-memory", byteQuantity(d.Facts.MemoryBytes)}
	if d.Facts.ScratchBytes > 0 {
		args = append(args, "--execution-ephemeral-storage", byteQuantity(d.Facts.ScratchBytes))
	}
	for _, cap := range d.Facts.Capabilities {
		args = append(args, "--execution-capability", cap)
	}
	return args
}

// Serve never retries a child start, even if its response is lost. Context is
// guest lifetime, not the provider/SDK session or admission call.
func Serve(ctx context.Context, c net.Conn, cfg Config) error {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	m, e := protocol.Read(c)
	if e != nil || m.Kind != "deliver" || m.Delivery == nil {
		return ErrAgent
	}
	d := *m.Delivery
	if cfg.LaunchID == "" || cfg.Identity == "" || d.LaunchID != cfg.LaunchID || d.Identity != cfg.Identity || len(d.Lease) == 0 || d.Job.JobID == "" || d.Job.JobDB == "" || d.Facts.Base == "" {
		return ErrAgent
	}
	if cfg.NestedKVM != slices.Contains(d.Facts.Capabilities, "kvm") {
		return ErrAgent
	}
	if e = state.PrivateDir(cfg.IntentDir); e != nil {
		return ErrAgent
	}
	if state.Fence(cfg.IntentDir+"/worker-intent") != nil {
		return ErrAgent
	}
	cmd := exec.CommandContext(ctx, cfg.C2J, Args(d)...)
	path := "PATH=/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:/usr/local/bin:/usr/bin:/bin"
	if cfg.NestedKVM {
		path += ":/usr/local/sbin:/usr/sbin:/sbin"
	}
	cmd.Env = []string{path, "HOME=/home/worker", "USER=worker", "LOGNAME=worker"}
	cmd.Dir = cfg.WorkDir
	if cmd.Dir == "" {
		cmd.Dir = "/home/worker"
	}
	if cfg.UID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: cfg.UID, Gid: cfg.GID}}
	}
	if cfg.NestedKVM {
		if cfg.UID != 1000 || cfg.GID != 1000 || setupKVM(ctx) != nil {
			return ErrAgent
		}
		nestedWorker(cmd)
	}
	cmd.Stdin = strings.NewReader(string(d.Lease))
	d.Lease = nil
	m.Delivery = nil
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return ErrAgent
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		return ErrAgent
	}
	if cmd.Start() != nil {
		return ErrAgent
	}
	frames := make(chan protocol.Message, 64)
	writerDone := make(chan struct{})
	abort := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(abort); c.Close() }) }
	go func() {
		defer close(writerDone)
		for frame := range frames {
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			if protocol.Write(c, frame) != nil {
				stop()
				return
			}
		}
	}()
	send := func(m protocol.Message) {
		select {
		case <-abort:
			return
		default:
		}
		select {
		case frames <- m:
		default:
			stop()
		}
	}
	send(protocol.Message{Kind: "worker-started"})
	var wg sync.WaitGroup
	drain := func(r io.Reader, kind string) {
		defer wg.Done()
		b := make([]byte, 4096)
		for {
			n, e := r.Read(b)
			if n > 0 {
				send(protocol.Message{Kind: kind, Data: append([]byte(nil), b[:n]...)})
			}
			if e != nil {
				return
			}
		}
	}
	wg.Add(2)
	go drain(stdout, "stdout")
	go drain(stderr, "stderr")
	wg.Wait()
	err := cmd.Wait()
	send(protocol.Message{Kind: "exit", Data: []byte(fmt.Sprint(exitCode(err)))})
	close(frames)
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		stop()
		<-writerDone
	}
	return nil
}
func exitCode(e error) int {
	if e == nil {
		return 0
	}
	var x *exec.ExitError
	if errors.As(e, &x) {
		return x.ExitCode()
	}
	return -1
}

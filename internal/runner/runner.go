package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

// Execute is called only by the dedicated systemd runner command. Its native
// process is never derived from a provider/RPC/SDK context.
func Execute(dir, digest string) error { return execute(dir, digest, connectGuest) }
func execute(dir, digest string, connect func(string, <-chan struct{}) (net.Conn, error)) error {
	if !filepath.IsAbs(dir) || state.PrivateDir(dir) != nil {
		return ErrRunner
	}
	s, e := ReadSpec(dir)
	if e != nil || Digest(s) != digest {
		return ErrRunner
	}
	boot, e := BootID()
	if e != nil || boot != s.BootID {
		return ErrRunner
	}
	lock, e := state.Lock(filepath.Join(dir, "runner.lock"))
	if e != nil {
		return ErrRunner
	}
	defer lock.Close()
	// A runner can never make another native start after this durable fence.
	if _, e = os.Lstat(filepath.Join(dir, "start.fence")); !os.IsNotExist(e) {
		return ErrRunner
	}
	c, e := net.DialTimeout("unix", filepath.Join(dir, "handoff"), 30*time.Second)
	if e != nil {
		return ErrRunner
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok || protocol.SameUser(uc) != nil {
		return ErrRunner
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	msg, e := protocol.Read(c)
	if e != nil || msg.Kind != "deliver" || msg.Delivery == nil || msg.Delivery.Identity != s.Identity || msg.Delivery.LaunchID != s.LaunchID {
		return ErrRunner
	}
	delivery := *msg.Delivery
	msg.Delivery = nil
	if delivery.Job != s.Job || !reflect.DeepEqual(delivery.Facts, s.Facts) || delivery.Platform != s.Platform || len(delivery.Lease) == 0 {
		return ErrRunner
	}
	for _, a := range [][2]string{{s.Hypervisor, s.HypervisorDigest}, {s.Kernel, s.KernelDigest}, {s.Source, s.SourceDigest}} {
		if images.Verify(a[0], a[1]) != nil {
			return ErrRunner
		}
	}
	if s.Initramfs != "" && images.Verify(s.Initramfs, s.InitramfsDigest) != nil {
		return ErrRunner
	}
	disk := filepath.Join(dir, "disk.raw")
	if images.Clone(s.Source, disk, s.DiskBytes) != nil {
		return ErrRunner
	}
	if state.Fence(filepath.Join(dir, "start.fence")) != nil {
		return ErrRunner
	}
	status := Status{Version: 1, Identity: s.Identity, Phase: "dispatching"}
	if state.Atomic(filepath.Join(dir, "status.json"), status, nil) != nil {
		return ErrRunner
	}
	if s.TimeoutSeconds > 0 && ArmBackstop(s) != nil {
		return ErrRunner
	}
	started := time.Now()
	var ctx context.Context = context.Background()
	cancel := func() {}
	if s.TimeoutSeconds > 0 {
		ctx, cancel = context.WithDeadline(ctx, started.Add(time.Duration(s.TimeoutSeconds)*time.Second))
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Hypervisor, Args(dir, s, started)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Start() != nil {
		status.Phase = "terminated"
		_ = state.Atomic(filepath.Join(dir, "status.json"), status, nil)
		return ErrRunner
	}
	status.Phase = "started"
	status.Started = started.UTC()
	if state.Atomic(filepath.Join(dir, "status.json"), status, nil) != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ErrRunner
	}
	// ACK means native process dispatch, not a successful boot or worker start.
	if protocol.Write(c, protocol.Message{Kind: "accepted"}) != nil {
		c.Close()
	}
	_ = c.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	output := make(chan protocol.Message, 64)
	var observer sync.Once
	disconnect := func() { observer.Do(func() { c.Close() }) }
	go func() {
		for m := range output {
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			if protocol.Write(c, m) != nil {
				disconnect()
				return
			}
		}
	}()
	emit := func(m protocol.Message) {
		m.Delivery = nil
		if m.Kind == "stdout" || m.Kind == "stderr" {
			for len(m.Data) > 4096 {
				select {
				case output <- protocol.Message{Kind: m.Kind, Data: append([]byte(nil), m.Data[:4096]...)}:
				default:
					disconnect()
				}
				m.Data = m.Data[4096:]
			}
		}
		select {
		case output <- m:
		default:
			disconnect()
		}
	}

	// Observe guest delivery once. No reconnect/resend after an uncertain write.
	guest, e := connect(filepath.Join(dir, "vsock"), done)
	if e == nil {
		_ = guest.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if protocol.Write(guest, protocol.Message{Kind: "deliver", Delivery: &delivery}) == nil {
			delivery.Lease = nil
			_ = guest.SetDeadline(time.Time{})
			go func() { <-done; guest.Close() }()
			for {
				m, e := protocol.Read(guest)
				if e != nil {
					emit(protocol.Message{Kind: "interrupted"})
					break
				}
				if m.Kind == "stdout" || m.Kind == "stderr" || m.Kind == "worker-started" || m.Kind == "exit" {
					emit(m)
				}
				if m.Kind == "exit" {
					break
				}
			}
		}
		guest.Close()
	} else {
		emit(protocol.Message{Kind: "unavailable"})
	}
	delivery.Lease = nil
	<-done
	status.Phase = "terminated"
	_ = state.Atomic(filepath.Join(dir, "status.json"), status, nil)
	emit(protocol.Message{Kind: "closed"})
	close(output)
	return nil
}
func Args(dir string, s Spec, dispatch time.Time) []string {
	// Network resources were provisioned by the operator. CH only attaches the
	// reserved TAP; it never creates bridges, routes or firewall rules.
	// v55 requires an explicit image type. Both prepared sources and job clones
	// are raw files; never ask the VMM to infer a backing format from their bytes.
	cmdline := []string{
		"root=/dev/vda1", "rw", "console=ttyS0", "net.ifnames=0",
		"pulse.clock=" + strconv.FormatInt(dispatch.Unix(), 10),
		"systemd.machine_id=" + s.Identity, "pulse.launch=" + s.LaunchID, "pulse.identity=" + s.Identity,
		"ip=" + s.Address + "::" + s.Gateway + ":" + s.Netmask + ":worker:eth0:off:" + s.DNS,
	}
	args := []string{
		"--kernel", s.Kernel,
		"--disk", "path=" + filepath.Join(dir, "disk.raw") + ",image_type=raw",
		"--cpus", "boot=" + strconv.Itoa(s.VCPUs),
		"--memory", "size=" + strconv.FormatInt(s.MemoryBytes, 10),
		"--net", "tap=" + s.TAP + ",mac=" + s.MAC,
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", s.CID, filepath.Join(dir, "vsock")),
		"--api-socket", filepath.Join(dir, "api"), "--serial", "off", "--console", "off",
		"--cmdline", strings.Join(cmdline, " "),
	}
	if s.Initramfs != "" {
		args = append(args, "--initramfs", s.Initramfs)
	}
	return args
}
func connectGuest(path string, done <-chan struct{}) (net.Conn, error) {
	return waitGuest(path, done, time.Minute)
}

func waitGuest(path string, done <-chan struct{}, timeout time.Duration) (net.Conn, error) {
	end := time.Now().Add(timeout)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return nil, ErrRunner
		case <-deadline.C:
			return nil, ErrRunner
		case <-tick.C:
			remaining := time.Until(end)
			if remaining <= 0 {
				return nil, ErrRunner
			}
			c, e := net.DialTimeout("unix", path, min(remaining, time.Second))
			if e != nil {
				continue
			}
			_ = c.SetDeadline(time.Now().Add(min(time.Until(end), 5*time.Second)))
			_, e = fmt.Fprintf(c, "CONNECT %d\n", protocol.Port)
			if e != nil {
				c.Close()
				continue
			}
			line, e := bufio.NewReader(io.LimitReader(c, 128)).ReadString('\n')
			if e != nil || !strings.HasPrefix(line, "OK ") {
				c.Close()
				continue
			}
			_ = c.SetDeadline(time.Time{})
			return c, nil
		}
	}
}

// Source maps only known output types; no assignment metadata is emitted.
func Source(kind string) api.LogSource {
	switch kind {
	case "stdout":
		return api.LogSourceStdout
	case "stderr":
		return api.LogSourceStderr
	default:
		return api.LogSourceLaunch
	}
}

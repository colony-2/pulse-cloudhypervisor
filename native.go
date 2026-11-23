package cloudhypervisor

import (
	"context"
	"errors"
	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

var ErrObservation = errors.New("cloudhypervisor: native identity or observation unavailable")

type observation struct {
	state      string
	terminated bool
}
type native interface {
	preflight(context.Context, int) error
	start(context.Context, string, runner.Spec, api.Lease, *subscription) (bool, error)
	observe(context.Context, string, runner.Spec) (observation, error)
}
type systemd struct{ config Config }

func (n systemd) preflight(ctx context.Context, slot int) error {
	c := n.config
	if slot < 0 || slot >= len(c.Network) || c.Runner == "" || c.SystemdRun == "" || c.Systemctl == "" || !validAsset(c.Hypervisor) || runtime.NumCPU() < c.VCPUs {
		return ErrObservation
	}
	f, e := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if e != nil {
		return ErrObservation
	}
	v, e := unix.IoctlRetInt(int(f.Fd()), 0xae00)
	f.Close()
	if e != nil || v != 12 {
		return ErrObservation
	}
	if images.Verify(c.Hypervisor.Path, c.Hypervisor.SHA256) != nil {
		return ErrObservation
	}
	for _, path := range []string{c.Runner, c.SystemdRun, c.Systemctl} {
		info, e := os.Stat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return ErrObservation
		}
	}
	iface, e := net.InterfaceByName(c.Network[slot].TAP)
	if e != nil || iface.Flags&net.FlagUp == 0 {
		return ErrObservation
	}
	var fs unix.Statfs_t
	if unix.Statfs(c.StateDir, &fs) != nil || fs.Bsize <= 0 || fs.Bavail > uint64(^uint64(0)>>1)/uint64(fs.Bsize) || int64(fs.Bavail)*fs.Bsize < c.DiskBytes {
		return ErrObservation
	}
	mem, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return ErrObservation
	}
	available := int64(0)
	for _, line := range strings.Split(string(mem), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" {
			available, _ = strconv.ParseInt(fields[1], 10, 64)
			available *= 1024
		}
	}
	if available < c.MemoryBytes {
		return ErrObservation
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append(n.managerArgs(), "show", "--property=Version", "--value")
	b, e := exec.CommandContext(probe, c.Systemctl, args...).Output()
	if e != nil || len(strings.TrimSpace(string(b))) == 0 {
		return ErrObservation
	}
	return nil
}

func (n systemd) managerArgs() []string {
	if n.config.UserManager {
		return []string{"--user"}
	}
	return nil
}
func (n systemd) start(ctx context.Context, dir string, s runner.Spec, lease api.Lease, sub *subscription) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr := &net.UnixAddr{Name: filepath.Join(dir, "handoff"), Net: "unix"}
	listener, e := net.ListenUnix("unix", addr)
	if e != nil {
		return false, ErrObservation
	}
	defer listener.Close()
	defer os.Remove(addr.Name)
	if os.Chmod(addr.Name, 0600) != nil {
		return false, ErrObservation
	}
	args := n.launchArgs(dir, s)
	cmd := exec.CommandContext(ctx, n.config.SystemdRun, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Any system-manager call may already have committed despite a lost response.
	if cmd.Run() != nil {
		return false, ErrObservation
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = listener.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	c, e := listener.AcceptUnix()
	if e != nil {
		return false, ErrObservation
	}
	if protocol.SameUser(c) != nil {
		c.Close()
		return false, ErrObservation
	}
	_ = c.SetDeadline(deadline)
	delivery := protocol.Delivery{LaunchID: s.LaunchID, Identity: s.Identity, Job: s.Job, Lease: []byte(lease), Facts: s.Facts, Platform: s.Platform}
	if protocol.Write(c, protocol.Message{Kind: "deliver", Delivery: &delivery}) != nil {
		c.Close()
		return false, ErrObservation
	}
	delivery.Lease = nil
	reply, e := protocol.Read(c)
	if e != nil || reply.Kind != "accepted" {
		c.Close()
		return false, ErrObservation
	}
	_ = c.SetDeadline(time.Time{})
	if sub == nil {
		c.Close()
		return true, nil
	}
	sub.source(s.LaunchID, api.LogSourceLaunch, api.SourceActive)
	sub.emit(s.LaunchID, api.LogSourceLaunch, []byte("native dispatch acknowledged"))
	go func() {
		select {
		case <-sub.Done():
			c.Close()
		}
	}()
	go func() {
		defer c.Close()
		for {
			m, e := protocol.Read(c)
			if e != nil {
				sub.finish(api.ListenerTransportLost, api.ErrLogTransport)
				return
			}
			switch m.Kind {
			case "stdout", "stderr":
				sub.emit(s.LaunchID, runner.Source(m.Kind), m.Data)
			case "worker-started":
				sub.source(s.LaunchID, api.LogSourceStdout, api.SourceActive)
				sub.source(s.LaunchID, api.LogSourceStderr, api.SourceActive)
				sub.emit(s.LaunchID, api.LogSourceLaunch, []byte("guest worker intent committed"))
			case "exit":
				// The guest sends c2j's exit code before closing the output stream.
				// Only these fixed messages reach launch logs.
				code, parseErr := strconv.Atoi(string(m.Data))
				if parseErr == nil && code == 0 {
					sub.emit(s.LaunchID, api.LogSourceLaunch, []byte("guest worker exited successfully"))
				} else if parseErr == nil && (code == -1 || code > 0 && code <= 255) {
					sub.emit(s.LaunchID, api.LogSourceLaunch, []byte("guest worker exited unsuccessfully"))
				}
			case "unavailable":
				sub.source(s.LaunchID, api.LogSourceStdout, api.SourceUnavailable)
				sub.source(s.LaunchID, api.LogSourceStderr, api.SourceUnavailable)
			case "interrupted":
				sub.finish(api.ListenerSourceError, api.ErrLogSource)
				return
			case "closed":
				for _, src := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
					sub.source(s.LaunchID, src, api.SourceClosed)
				}
				return
			}
		}
	}()
	return true, nil
}

// Cloud Hypervisor v55 uses an empty OFD-locked file next to its API socket.
// SIGKILL leaves it behind. Only take the exact sidecar of a terminated VM,
// retaining the same whole-file lock until its directory has been cleaned.
func lockAPISidecar(dir string) (*os.File, error) {
	path := filepath.Join(dir, "api.lock")
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, state.ErrUnsafe
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 ||
		st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 || st.Size != 0 ||
		unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lock) != nil {
		f.Close()
		return nil, state.ErrUnsafe
	}
	return f, nil
}

func removeAPISidecar(dir string, lock *os.File) error {
	if lock == nil {
		if _, err := os.Lstat(filepath.Join(dir, "api.lock")); os.IsNotExist(err) {
			return nil
		}
		return state.ErrUnsafe
	}
	return removeSameFile(lock)
}

func removeSameFile(file *os.File) error {
	held, err := file.Stat()
	current, pathErr := os.Lstat(file.Name())
	if err != nil || pathErr != nil || !os.SameFile(held, current) {
		return state.ErrUnsafe
	}
	if os.Remove(file.Name()) != nil {
		return state.ErrStorage
	}
	return nil
}

func durationSeconds(n int64) string { return time.Duration(n * time.Second.Nanoseconds()).String() }
func (n systemd) observe(ctx context.Context, dir string, s runner.Spec) (observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	boot, e := runner.BootID()
	if e != nil {
		return observation{}, ErrObservation
	}
	// A changed boot ID proves previous native processes cannot survive. Reopen
	// never recreates units, dispatches work or recovers credentials.
	if boot != s.BootID {
		return observation{terminated: true}, nil
	}
	args := append(n.managerArgs(), "show", s.Unit, "--property=Description,ActiveState,SubState,ControlGroup", "--no-pager")
	b, e := exec.CommandContext(ctx, n.config.Systemctl, args...).Output()
	if e != nil {
		return observation{}, ErrObservation
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	if fields["Description"] != "pulse:"+s.Identity {
		return observation{}, ErrObservation
	}
	active := fields["ActiveState"]
	sub := fields["SubState"]
	if active == "active" && sub == "running" {
		return observation{state: "running"}, nil
	}
	if active == "activating" {
		return observation{state: "starting"}, nil
	}
	if active == "deactivating" {
		return observation{state: "stopping"}, nil
	}
	// The runner lock excludes a native start still in progress. A terminal
	// status alone is insufficient: inspect the exact systemd cgroup too.
	if !(active == "inactive" || active == "failed" || sub == "exited") {
		return observation{}, ErrObservation
	}
	lock, e := state.ExistingLock(filepath.Join(dir, "runner.lock"))
	if e != nil {
		return observation{}, ErrObservation
	}
	defer lock.Close()
	group := fields["ControlGroup"]
	if group != "" {
		if !strings.HasPrefix(group, "/") || filepath.Clean(group) != group {
			return observation{}, ErrObservation
		}
		events, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.events"))
		if e != nil || !strings.Contains(string(events), "populated 0\n") {
			return observation{}, ErrObservation
		}
	}
	return observation{terminated: true}, nil
}

func (n systemd) launchArgs(dir string, s runner.Spec) []string {
	// On runner loss systemd must keep supervising the VMM until it exits
	// or the independently armed runtime limit expires.
	args := append(n.serviceArgs(), "--property=ExitType=cgroup")
	args = append(args, "--no-block", "--quiet", "--unit="+s.Unit, "--property=Description=pulse:"+s.Identity, "--property=KillMode=control-group", "--property=KillSignal=SIGKILL", "--property=SendSIGKILL=yes", "--property=TimeoutStopSec=5s", "--property=RemainAfterExit=yes", "--property=Restart=no", "--property=StandardOutput=null", "--property=StandardError=null", "--property=UMask=0077")
	if s.TimeoutSeconds > 0 {
		args = append(args, "--service-type=notify", "--property=NotifyAccess=main", "--property=TimeoutStartSec=1800s",
			"--property=RuntimeMaxSec="+durationSeconds(s.TimeoutSeconds))
	} else {
		args = append(args, "--service-type=exec")
	}
	args = append(args, "--", n.config.Runner, "runner", "--dir", dir, "--digest", runner.Digest(s))
	return args
}

// System services otherwise default to root, which breaks the authenticated
// same-user handoff and gives the VMM unnecessary privilege. User managers
// already fix the service identity and cannot set User/Group properties.
func (n systemd) serviceArgs() []string {
	if n.config.UserManager {
		return []string{"--user"}
	}
	return []string{"--property=User=" + strconv.Itoa(os.Geteuid()), "--property=Group=" + strconv.Itoa(os.Getegid())}
}

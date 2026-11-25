//go:build live

package live

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"golang.org/x/sys/unix"
)

// Open before reading procfs so a recycled PID cannot become a signal target.
func pidHandle(t *testing.T, pid int, group string, argv []string) int {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal("open process identity")
	}
	t.Cleanup(func() { unix.Close(fd) })
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	st, err := os.Stat(dir)
	if err != nil || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Fatal("process owner mismatch")
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || !bytes.Equal(cmdline, []byte(strings.Join(argv, "\x00")+"\x00")) {
		t.Fatal("unexpected supervised process")
	}
	data, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil || string(data) != "0::"+group+"\n" {
		t.Fatal("process cgroup mismatch")
	}
	return fd
}

type ownedProcess struct{ pid, fd int }

func (e *executionFixture) ownedPair(t *testing.T, dir string, spec runner.Spec) (ownedProcess, ownedProcess) {
	t.Helper()
	boot, err := runner.BootID()
	if err != nil || boot != spec.BootID || spec.Owner != e.config.Owner || !e.config.UserManager {
		t.Fatal("unit owner or boot mismatch")
	}
	show := exec.CommandContext(e.ctx, e.config.Systemctl, "--user", "show", spec.Unit,
		"--property=Description,MainPID,ControlGroup")
	data, err := show.Output()
	if err != nil {
		t.Fatal("observe unit")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			fields[key] = value
		}
	}
	group := fields["ControlGroup"]
	main, err := strconv.Atoi(fields["MainPID"])
	if err != nil || main <= 1 || fields["Description"] != "pulse:"+spec.Identity ||
		!filepath.IsAbs(group) || filepath.Clean(group) != group || filepath.Base(group) != spec.Unit {
		t.Fatal("unit identity mismatch")
	}
	mainFD := pidHandle(t, main, group, []string{e.config.Runner, "runner", "--dir", dir, "--digest", runner.Digest(spec)})
	members, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.procs"))
	if err != nil || len(strings.Fields(string(members))) != 2 {
		t.Fatal("unexpected unit membership")
	}
	for _, entry := range strings.Fields(string(members)) {
		pid, err := strconv.Atoi(entry)
		if err != nil || pid <= 1 {
			t.Fatal("malformed cgroup membership")
		}
		if pid != main {
			status, err := runner.ReadStatus(dir, spec)
			if err != nil || status.Started.IsZero() {
				t.Fatal("verify guest dispatch clock")
			}
			vmFD := pidHandle(t, pid, group, append([]string{spec.Hypervisor}, runner.Args(dir, spec, status.Started)...))
			return ownedProcess{main, mainFD}, ownedProcess{pid, vmFD}
		}
	}
	t.Fatal("owned VMM unavailable")
	return ownedProcess{}, ownedProcess{}
}

func checkCap(t *testing.T, killRunner bool) {
	e := openExecution(t)
	foreign := cleanupCanary(t, e.config.StateDir)
	name := "cap-controller"
	if killRunner {
		name = "cap-runner"
	}
	l, _ := e.launch(t, name, "valid", "cap")
	l.TimeoutSeconds = 75
	out := new(guestOutput)
	e.submit(t, l, out.listen(e.ctx, name))
	e.waitForWorker(t, out)
	e.waitForRenewal(t, name)
	dir := launchDirectory(e.config.StateDir, name)
	spec, err := runner.ReadSpec(dir)
	if err != nil || spec.Job != l.Job || spec.LaunchID != name {
		t.Fatal("missing owned deadline record")
	}
	status, err := runner.ReadStatus(dir, spec)
	if err != nil || status.Started.IsZero() {
		t.Fatal("dispatch time unavailable")
	}
	main, vm := e.ownedPair(t, dir, spec)
	if err := e.provider.Close(); err != nil {
		t.Fatal("close controller")
	}
	if killRunner && unix.PidfdSendSignal(main.fd, unix.SIGKILL, nil, 0) != nil {
		t.Fatal("signal owned runner")
	}
	expiry := status.Started.Add(time.Duration(l.TimeoutSeconds) * time.Second)
	till := time.Until(expiry.Add(-2 * time.Second))
	if till <= 0 {
		t.Fatal("missed pre-expiry observation window")
	}
	wait := []unix.PollFd{{Fd: int32(vm.fd), Events: unix.POLLIN}}
	for time.Until(expiry.Add(-2*time.Second)) > 0 {
		ms := max(1, int(time.Until(expiry.Add(-2*time.Second)).Milliseconds()))
		n, err := unix.Poll(wait, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n != 0 {
			job, _ := e.observe(e.ctx, name)
			t.Logf("Time before cap %s, JobDB state %s, renewals %d", time.Until(expiry).Round(time.Millisecond), job.State, job.Renewals)
			t.Fatal("VMM terminated before the dispatch-relative cap")
		}
	}
	for {
		ms := int(time.Until(expiry.Add(6 * time.Second)).Milliseconds())
		if ms < 0 {
			t.Fatal("VMM outlived its independent cap")
		}
		n, err := unix.Poll(wait, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n != 1 || wait[0].Revents&unix.POLLIN == 0 {
			t.Fatal("VMM termination unavailable")
		}
		break
	}
	e.reopen(t)
	e.waitForCleanup(t, l)
	e.assertTerminalCleanup(t, l, foreign)
	final, err := e.observe(e.ctx, name)
	if err != nil || final.Acquisitions != 1 || final.ClaimAttempts != 0 || final.OutputOK {
		t.Fatal("capped workload was replayed or finished early")
	}
}

func TestS17(t *testing.T) { checkCap(t, false) }
func TestS18(t *testing.T) { checkCap(t, true) }

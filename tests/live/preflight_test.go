//go:build live

package live

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"golang.org/x/sys/unix"
)

// This preflight never requests a lease. The test fixture is prepared inside the
// supplied allocation; missing reboot coordination must not block guest tests.
func TestNativePreflight(t *testing.T) {
	f, c, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil {
		t.Fatal("FAIL: authorized native fixture descriptor is required")
	}
	if !concurrentCapacity(c, runtime.NumCPU()) {
		t.Fatal("FAIL: concurrent guest and builder budgets are required")
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal("FAIL: KVM open")
	}
	version, err := unix.IoctlRetInt(int(kvm.Fd()), 0xae00)
	kvm.Close()
	if err != nil || version != 12 {
		t.Fatal("FAIL: KVM API")
	}
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal("FAIL: memory observation")
	}
	var available int64
	for _, line := range strings.Split(string(mem), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" {
			available, _ = strconv.ParseInt(fields[1], 10, 64)
			available *= 1024
		}
	}
	if available < 2*c.MemoryBytes {
		t.Fatal("FAIL: actual memory budget")
	}
	var fs unix.Statfs_t
	// State root is disposable and supplied by the fixture operator. No implicit
	// mkdir, networking, service installation or privilege widening occurs here.
	if unix.Statfs(c.StateDir, &fs) != nil || fs.Bsize <= 0 || fs.Bavail < uint64(3*c.DiskBytes/fs.Bsize+1) {
		t.Fatal("FAIL: actual disk budget")
	}
	if images.Verify(c.Hypervisor.Path, c.Hypervisor.SHA256) != nil || images.Verify(f.BootstrapAgent.Path, f.BootstrapAgent.SHA256) != nil {
		t.Fatal("FAIL: native executable digest")
	}
	matched := false
	for _, b := range c.Bases {
		if b.Architecture != runtime.GOARCH {
			continue
		}
		matched = true
		for _, a := range [][2]string{{b.Disk.Path, b.Disk.SHA256}, {b.Kernel.Path, b.Kernel.SHA256}, {b.Preparation.Path, b.Preparation.SHA256}} {
			if images.Verify(a[0], a[1]) != nil {
				t.Fatal("FAIL: native input digest")
			}
		}
		if b.Initramfs.Path != "" && images.Verify(b.Initramfs.Path, b.Initramfs.SHA256) != nil {
			t.Fatal("FAIL: initramfs digest")
		}
	}
	if !matched {
		t.Fatal("FAIL: native base")
	}
	for _, n := range c.Network {
		iface, e := net.InterfaceByName(n.TAP)
		if e != nil || iface.Flags&net.FlagUp == 0 {
			t.Fatal("FAIL: reserved TAP")
		}
		data, e := os.ReadFile(filepath.Join("/sys/class/net", n.TAP, "tun_flags"))
		if e != nil || len(data) == 0 {
			t.Fatal("FAIL: reserved TAP type")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if f.ready(ctx) != nil {
		t.Fatal("FAIL: protected fixture observation channel")
	}
	if f.guestFaultAuthority() != nil {
		t.Fatal("FAIL: disposable controller/runner fault authority")
	}
	probeSupervisor(t, ctx, c.SystemdRun, c.Systemctl, c.UserManager)
}

// Full acceptance still runs this test. This checks the declaration only; it
// neither reboots the host nor substitutes for LIVE-09's real recovery assertion.
func TestNativeRebootPreflight(t *testing.T) {
	f, _, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil || f.rebootFaultAuthority() != nil {
		t.Fatal("FAIL: separate Linux test-host reboot declaration is required")
	}
}

func probeSupervisor(t *testing.T, ctx context.Context, run, ctl string, user bool) {
	t.Helper()
	// The unit is unique, contains no guest/lease, and executes as this user.
	unit := "pulse-ch-preflight-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".service"
	args := []string{"--no-ask-password"}
	if user {
		args = append(args, "--user")
	}
	description := "pulse-native-preflight:" + unit
	command := func(binary string, argv ...string) ([]byte, error) {
		return exec.CommandContext(ctx, binary, argv...).Output()
	}
	version, err := command(ctl, append(append([]string{}, args...), "show", "--property=Version", "--value")...)
	if err != nil || strings.TrimSpace(string(version)) == "" {
		var exit *exec.ExitError
		if user && errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "No medium found") {
			t.Fatal("FAIL: systemctl --user show --property=Version: No medium found")
		}
		t.Fatal("FAIL: systemd manager query; no probe unit dispatched")
	}
	launch := append(append([]string{}, args...), "--quiet", "--no-block", "--service-type=exec", "--unit="+unit, "--property=Description="+description, "--property=RuntimeMaxSec=30s", "--property=KillMode=control-group", "--property=Restart=no", "--property=StandardOutput=null", "--property=StandardError=null")
	if !user {
		launch = append(launch, "--property=User="+strconv.Itoa(os.Geteuid()), "--property=Group="+strconv.Itoa(os.Getegid()))
	}
	launch = append(launch, "--", "/usr/bin/sleep", "30")
	launched := false
	// Register exact-owned cleanup before dispatch: a failed response may still
	// mean the manager created the unit. Never stop a unit with another identity.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		desc := exec.CommandContext(cleanup, ctl, append(append([]string{}, args...), "show", unit, "--property=Description", "--value")...)
		value, err := desc.Output()
		if err != nil || strings.TrimSpace(string(value)) != description {
			if launched {
				t.Error("FAIL: supervisor probe cleanup identity unavailable")
			}
			return
		}
		if exec.CommandContext(cleanup, ctl, append(append([]string{}, args...), "stop", unit)...).Run() != nil {
			t.Error("FAIL: supervisor probe cleanup")
		}
	}()
	if _, err := command(run, launch...); err != nil {
		t.Fatal("FAIL: independent same-user supervisor launch authority")
	}
	launched = true
	var fields map[string]string
	for i := 0; i < 50; i++ {
		data, err := command(ctl, append(append([]string{}, args...), "show", unit, "--property=Description,ActiveState,ControlGroup,User,KillMode,MainPID,Restart,RuntimeMaxUSec")...)
		if err != nil {
			t.Fatal("FAIL: supervisor inspection authority")
		}
		fields = map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				fields[k] = v
			}
		}
		if fields["ActiveState"] == "active" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if fields["Description"] != description || fields["ActiveState"] != "active" || fields["KillMode"] != "control-group" || fields["Restart"] != "no" || fields["RuntimeMaxUSec"] != "30s" || fields["ControlGroup"] == "" {
		t.Fatal("FAIL: supervisor properties")
	}
	if !user && fields["User"] != strconv.Itoa(os.Geteuid()) {
		t.Fatal("FAIL: supervisor execution user")
	}
	pid, err := strconv.Atoi(fields["MainPID"])
	if err != nil || pid <= 1 {
		t.Fatal("FAIL: supervisor process identity")
	}
	processDir := filepath.Join("/proc", strconv.Itoa(pid))
	processStatus, err := os.ReadFile(filepath.Join(processDir, "status"))
	if err != nil || !effectiveID(processStatus, "Uid:", os.Geteuid()) || !effectiveID(processStatus, "Gid:", os.Getegid()) {
		t.Fatal("FAIL: supervisor process UID/GID")
	}
	callerGroup, callerErr := os.ReadFile("/proc/self/cgroup")
	serviceGroup, serviceErr := os.ReadFile(filepath.Join(processDir, "cgroup"))
	if callerErr != nil || serviceErr != nil || !independentSupervisor(callerGroup, serviceGroup, fields["ControlGroup"]) {
		t.Fatal("FAIL: active service is not proven independent of the caller cgroup")
	}
	if _, err := command(ctl, append(append([]string{}, args...), "set-property", "--runtime", unit, "RuntimeMaxSec=20s")...); err != nil {
		t.Fatal("FAIL: supervisor runtime-property authority")
	}
	value, err := command(ctl, append(append([]string{}, args...), "show", unit, "--property=RuntimeMaxUSec", "--value")...)
	if err != nil || strings.TrimSpace(string(value)) != "20s" {
		t.Fatal("FAIL: supervisor runtime-property readback")
	}
}

func effectiveID(status []byte, label string, want int) bool {
	for _, line := range strings.Split(string(status), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 5 && parts[0] == label {
			return parts[2] == strconv.Itoa(want)
		}
	}
	return false
}

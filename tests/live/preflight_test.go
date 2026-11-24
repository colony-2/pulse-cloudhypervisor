//go:build live

package live

import (
	"context"
	"encoding/hex"
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

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	"golang.org/x/sys/unix"
)

// Check the supplied allocation before requesting a lease.
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
	// Setup checked capacity for the source and both full-size clones before
	// promotion. Once an immutable source occupies its reservation, only the two
	// writable copies need additional space.
	copies := int64(3)
	if len(c.Bases) == 1 && preparedDisk(c, c.Bases[0]) {
		copies = 2
	}
	if unix.Statfs(c.StateDir, &fs) != nil || fs.Bsize <= 0 ||
		fs.Bavail < uint64((copies*c.DiskBytes+fs.Bsize-1)/fs.Bsize) {
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

func preparedDisk(c ch.Config, base ch.Base) bool {
	fingerprint := ch.Fingerprint(base)
	sources := filepath.Join(c.StateDir, "sources")
	var recorded preparedMarker
	if state.Read(filepath.Join(sources, fingerprint+".json"), &recorded) != nil ||
		recorded.Version != 1 || recorded.Fingerprint != fingerprint || len(recorded.Digest) != 64 || recorded.Proof.Recipe != base.Recipe {
		return false
	}
	if _, err := hex.DecodeString(recorded.Digest); err != nil {
		return false
	}
	want := filepath.Join(sources, fingerprint+"-"+recorded.Digest[:12]+".raw")
	st, err := os.Lstat(want)
	if err != nil || recorded.Path != want || !st.Mode().IsRegular() || st.Mode().Perm() != 0400 || st.Size() != c.DiskBytes {
		return false
	}
	return images.Verify(want, recorded.Digest) == nil
}

func TestSupervisedNativeAccess(t *testing.T) {
	if os.Getenv("CH_SUPERVISED_ACCESS") != "1" {
		return
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal("supervised KVM access")
	}
	defer kvm.Close()
	vm, err := unix.IoctlRetInt(int(kvm.Fd()), 0xae01)
	if err != nil || unix.Close(vm) != nil {
		t.Fatal("supervised KVM VM creation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=Version", "--value").Output()
	if err != nil || strings.TrimSpace(string(version)) == "" {
		t.Fatal("supervised user manager access")
	}
	// The worker reserves the TAP and grants this UID attachment rights.
	name := os.Getenv("CH_SUPERVISED_TAP")
	iface, err := net.InterfaceByName(name)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		t.Fatal("reserved TAP is unavailable to the runner")
	}
	tun, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		t.Fatal("supervised tun access")
	}
	defer tun.Close()
	request, err := unix.NewIfreq(name)
	if err != nil {
		t.Fatal("reserved TAP name")
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if unix.IoctlIfreq(int(tun.Fd()), unix.TUNSETIFF, request) != nil ||
		unix.IoctlIfreq(int(tun.Fd()), unix.TUNGETIFF, request) != nil || request.Name() != name || request.Uint16()&unix.IFF_TAP == 0 {
		t.Fatal("supervised runner could not attach the reserved TAP")
	}
	after, err := net.InterfaceByName(name)
	if err != nil || after.Index != iface.Index {
		t.Fatal("reserved TAP identity changed")
	}
}

func TestS07(t *testing.T) {
	_, cfg, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil || len(cfg.Network) == 0 {
		t.Fatal("reserved fixture TAP required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Fatal("supervisor unavailable")
	}
	ctl, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatal("manager unavailable")
	}
	// Observe a separate cgroup, UID/GID, cap enforcement and runtime readback.
	probeSupervisor(t, ctx, run, ctl, true)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal("locate supervised access test")
	}
	unit := "pulse-ch-access-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".service"
	args := []string{"--user", "--wait", "--collect", "--no-ask-password", "--quiet", "--unit=" + unit,
		"--service-type=exec", "--property=RuntimeMaxSec=8s", "--property=KillMode=control-group",
		"--property=StandardOutput=null", "--property=StandardError=null", "--setenv=CH_SUPERVISED_ACCESS=1",
		"--setenv=CH_SUPERVISED_TAP=" + cfg.Network[0].TAP,
		"--", bin, "-test.run=^TestSupervisedNativeAccess$"}
	if exec.CommandContext(ctx, run, args...).Run() != nil {
		t.Fatal("supervisor lost KVM, TAP or user-manager access")
	}
}

func TestS09(t *testing.T) {
	f, baseConfig, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil {
		t.Fatal("native measured fixture required")
	}
	for _, fault := range []string{"manager", "disk-budget", "hypervisor-digest"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// Use an independent provider owner. Observe every builder intent and
			// launch through its own durable state, leaving qualified sources intact.
			cfg := baseConfig
			cfg.Owner = "negative-" + fault
			cfg.StateDir, err = os.MkdirTemp("/tmp", "ch-negative-")
			if err != nil {
				t.Fatal("private negative preflight scope")
			}
			switch fault {
			case "manager":
				cfg.Systemctl = filepath.Join(cfg.StateDir, "absent-systemctl")
			case "disk-budget":
				cfg.HostDiskBytes = cfg.DiskBytes - 1
			case "hypervisor-digest":
				cfg.Hypervisor.SHA256 = strings.Repeat("0", 64)
			}
			provider, err := ch.New(cfg)
			if err != nil {
				t.Fatal("construct negative preflight owner")
			}
			defer provider.Close()
			if err := provider.Setup(ctx); !errors.Is(err, ch.ErrPreparation) || ctx.Err() != nil {
				t.Fatal("prerequisite failure did not reject setup before dispatch")
			}
			for _, name := range []string{"jobs", "sources"} {
				children, err := os.ReadDir(filepath.Join(cfg.StateDir, name))
				if (err != nil && !os.IsNotExist(err)) || len(children) != 0 {
					t.Fatal("unexpected launch or preparation record")
				}
			}
			if provider.Close() != nil || os.RemoveAll(cfg.StateDir) != nil {
				t.Fatal("release empty negative scope")
			}
			if f.ready(ctx) != nil {
				t.Fatal("independent assignment fixture became unavailable")
			}
		})
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
	if _, err := command(ctl, append(append([]string{}, args...), "set-property", "--runtime", unit, "CPUWeight=99")...); err != nil {
		t.Fatal("FAIL: supervisor runtime-property authority")
	}
	value, err := command(ctl, append(append([]string{}, args...), "show", unit, "--property=CPUWeight", "--value")...)
	if err != nil || strings.TrimSpace(string(value)) != "99" {
		t.Fatal("FAIL: supervisor runtime-property readback")
	}
	probeCapActivation(t, run, ctl, user)
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

// This helper runs as the main process of a dedicated transient test unit. It
// exercises the same readiness notification and cap readback as the real runner.
func TestNativeNotifyHelper(t *testing.T) {
	id := os.Getenv("CH_CAP_PROBE_ID")
	if id == "" {
		return
	}
	if _, err := strconv.ParseInt(id, 36, 64); err != nil {
		t.Fatal("invalid cap probe")
	}
	time.Sleep(4 * time.Second)
	s := runner.Spec{Unit: "pulse-ch-cap-" + id + ".service", Identity: id, TimeoutSeconds: 2,
		Systemctl: os.Getenv("CH_CAP_PROBE_CTL"), UserManager: os.Getenv("CH_CAP_PROBE_USER") == "1"}
	if runner.ArmBackstop(s) != nil {
		t.Fatal("arm dispatch-relative cap")
	}
	time.Sleep(20 * time.Second)
	t.Fatal("supervisor failed to enforce the cap")
}

func probeCapActivation(t *testing.T, run, ctl string, user bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Second)
	defer cancel()
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	unit := "pulse-ch-cap-" + id + ".service"
	description := "pulse:" + id
	managed := []string{"--no-ask-password"}
	userFlag := "0"
	if user {
		managed = append(managed, "--user")
		userFlag = "1"
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal("locate cap probe")
	}
	args := append(append([]string{}, managed...), "--quiet", "--no-block", "--service-type=notify", "--unit="+unit,
		"--property=Description="+description, "--property=NotifyAccess=main", "--property=TimeoutStartSec=12s",
		"--property=RuntimeMaxSec=2s", "--property=ExitType=cgroup", "--property=KillMode=control-group",
		"--property=KillSignal=SIGKILL", "--property=Restart=no", "--property=StandardOutput=null", "--property=StandardError=null",
		"--setenv=CH_CAP_PROBE_ID="+id, "--setenv=CH_CAP_PROBE_CTL="+ctl, "--setenv=CH_CAP_PROBE_USER="+userFlag)
	if !user {
		args = append(args, "--property=User="+strconv.Itoa(os.Geteuid()), "--property=Group="+strconv.Itoa(os.Getegid()))
	}
	args = append(args, "--", bin, "-test.run=^TestNativeNotifyHelper$")
	query := func(ctx context.Context, props string) string {
		out, err := exec.CommandContext(ctx, ctl, append(append([]string{}, managed...), "show", unit, "--property="+props)...).Output()
		if err != nil {
			return ""
		}
		return string(out)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 4*time.Second)
		defer done()
		if strings.TrimSpace(query(stop, "Description")) == "Description="+description {
			if exec.CommandContext(stop, ctl, append(append([]string{}, managed...), "stop", unit)...).Run() != nil {
				t.Error("cap probe cleanup")
			}
		}
	}()
	issued := time.Now()
	if exec.CommandContext(ctx, run, args...).Run() != nil {
		t.Fatal("start cap probe")
	}
	var active time.Time
	for ctx.Err() == nil {
		state := query(ctx, "Description,ActiveState,Result,RuntimeMaxUSec")
		if strings.Contains(state, "Description="+description+"\n") && strings.Contains(state, "RuntimeMaxUSec=2s\n") {
			if active.IsZero() && strings.Contains(state, "ActiveState=active\n") {
				active = time.Now()
			}
			if strings.Contains(state, "Result=timeout\n") && strings.Contains(state, "ActiveState=failed\n") {
				if active.IsZero() || active.Sub(issued) < 3*time.Second || time.Since(active) < time.Second {
					t.Fatal("cap did not start at readiness")
				}
				return
			}
		}
		time.Sleep(70 * time.Millisecond)
	}
	t.Fatal("owned service did not expire after measured readiness")
}

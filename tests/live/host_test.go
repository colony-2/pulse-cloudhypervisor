//go:build live

package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeAllocationAccess(t *testing.T) {
	t.Logf("execution %s/%s as UID/GID %d/%d", runtime.GOOS, runtime.GOARCH, os.Geteuid(), os.Getegid())
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Fatal("native Linux ARM64 is required")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(strings.TrimSpace(string(bootID))) != 36 {
		t.Fatal("Linux boot ID is unavailable")
	}
	t.Logf("Linux boot ID %s", strings.TrimSpace(string(bootID)))
	processStatus, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal("process capabilities are unavailable")
	}
	for _, line := range strings.Split(string(processStatus), "\n") {
		if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "CapAmb:") {
			t.Log(line)
		}
	}
	t.Run("TAP", probeTAP)
	t.Run("UserManager", func(t *testing.T) {
		run, runErr := exec.LookPath("systemd-run")
		ctl, ctlErr := exec.LookPath("systemctl")
		if runErr != nil || ctlErr != nil {
			t.Fatal("systemd-run and systemctl are required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		probeSupervisor(t, ctx, run, ctl, true)
	})
}

func probeTAP(t *testing.T) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("TAP identity generation failed")
	}
	name := "ch" + hex.EncodeToString(suffix[:])
	if _, err := net.InterfaceByName(name); err == nil {
		t.Fatal("probe TAP identity already exists")
	}
	tun, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/net/tun: %v", err)
	}
	defer tun.Close()
	request, err := unix.NewIfreq(name)
	if err != nil {
		t.Fatal("invalid probe TAP name")
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_TUN_EXCL)
	if err = unix.IoctlIfreq(int(tun.Fd()), unix.TUNSETIFF, request); err != nil {
		t.Fatalf("TUNSETIFF %s IFF_TAP|IFF_NO_PI|IFF_TUN_EXCL: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(filepath.Join("/sys/class/net", name)); !errors.Is(err, os.ErrNotExist) {
			t.Error("probe TAP removal could not be verified after releasing its only file descriptor")
		}
	})
	if err = unix.IoctlIfreq(int(tun.Fd()), unix.TUNGETIFF, request); err != nil || request.Name() != name || request.Uint16()&unix.IFF_TAP == 0 {
		t.Fatal("owned TAP identity could not be verified through its descriptor")
	}
	if _, err = os.Stat(filepath.Join("/sys/class/net", name, "tun_flags")); err != nil {
		t.Fatal("owned TAP is not visible in the allocation")
	}
	if err = tun.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned TAP descriptor close failed")
	}
}

// TestS06 observes the allocation's existing interfaces and routes around the
// real, exclusive TAP lifecycle. The probe never writes a worker route.
func TestS06(t *testing.T) {
	snapshot := func() (map[string]int, []string) {
		t.Helper()
		ifaces, err := net.Interfaces()
		if err != nil || len(ifaces) == 0 {
			t.Fatal("inspect allocation interfaces")
		}
		indices := make(map[string]int, len(ifaces))
		for _, iface := range ifaces {
			indices[iface.Name] = iface.Index
		}
		var routes []string
		for _, table := range []string{"/proc/net/route", "/proc/net/ipv6_route"} {
			data, err := os.ReadFile(table)
			if err != nil {
				t.Fatal("inspect allocation routes")
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				f := strings.Fields(line)
				if len(f) == 0 || f[0] == "Iface" {
					continue
				}
				if table == "/proc/net/route" && len(f) == 11 {
					routes = append(routes, "v4 "+strings.Join(append(f[:4:4], f[6:]...), " "))
				} else if table == "/proc/net/ipv6_route" && len(f) == 10 {
					routes = append(routes, "v6 "+strings.Join(append(f[:6:6], f[8:]...), " "))
				} else {
					t.Fatal("unexpected kernel route format")
				}
			}
		}
		sort.Strings(routes)
		return indices, routes
	}
	before, routes := snapshot()
	probeTAP(t)
	after, next := snapshot()
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(routes, next) {
		t.Fatal("unrelated interfaces or routes changed during TAP probe")
	}
}

// TestNativeHost needs only the provided ARM64/KVM worker and installed VMM.
// It creates an empty KVM VM and exercises an owned VMM control process. It
// does not boot a guest, use a lease, alter networking or establish LIVE coverage.
func TestNativeHost(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Fatal("native Linux ARM64 is required")
	}
	t.Run("KVM", func(t *testing.T) {
		kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			t.Fatal("KVM open failed")
		}
		defer kvm.Close()
		version, err := unix.IoctlRetInt(int(kvm.Fd()), 0xae00) // KVM_GET_API_VERSION
		if err != nil || version != 12 {
			t.Fatal("KVM API version is unavailable or unsupported")
		}
		vm, err := unix.IoctlRetInt(int(kvm.Fd()), 0xae01) // KVM_CREATE_VM, default VM type
		if err != nil {
			t.Fatal("KVM VM creation failed")
		}
		if unix.Close(vm) != nil {
			t.Fatal("owned KVM VM close failed")
		}
	})
	t.Run("HypervisorControl", func(t *testing.T) {
		binary, err := exec.LookPath("cloud-hypervisor")
		if err != nil {
			t.Fatal("installed Cloud Hypervisor is required")
		}
		// Keep the Unix socket path short and its directory private regardless
		// of the checkout path, inherited TMPDIR or umask.
		dir, err := os.MkdirTemp("/tmp", "ch-host-")
		if err != nil {
			t.Fatal("private control directory creation failed")
		}
		t.Cleanup(func() {
			if os.RemoveAll(dir) != nil {
				t.Error("owned control directory cleanup failed")
			}
		})
		socket := filepath.Join(dir, "api")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--api-socket", socket)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if cmd.Start() != nil {
			t.Fatal("Cloud Hypervisor control process start failed")
		}
		done := make(chan struct{})
		var waitErr error
		go func() { waitErr = cmd.Wait(); close(done) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned control process cleanup timed out")
			}
		})
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: time.Second}
		ready := false
		for !ready {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api/v1/vmm.ping", nil)
			response, err := client.Do(req)
			if err == nil {
				var ping struct {
					Version string `json:"version"`
				}
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&ping)
				response.Body.Close()
				if response.StatusCode != http.StatusOK || decodeErr != nil || ping.Version == "" {
					t.Fatal("Cloud Hypervisor ping response is invalid")
				}
				ready = true
				continue
			}
			select {
			case <-ctx.Done():
				t.Fatal("Cloud Hypervisor control socket readiness timed out")
			case <-done:
				t.Fatal("Cloud Hypervisor exited before control socket readiness")
			case <-time.After(20 * time.Millisecond):
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vmm.shutdown", nil)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("Cloud Hypervisor shutdown request failed")
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("Cloud Hypervisor shutdown returned HTTP %d", response.StatusCode)
		}
		select {
		case <-done:
			if waitErr != nil {
				t.Fatal("Cloud Hypervisor control process exited unsuccessfully")
			}
		case <-ctx.Done():
			t.Fatal("Cloud Hypervisor control process did not exit after shutdown")
		}
	})
}

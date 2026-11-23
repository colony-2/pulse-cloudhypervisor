package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fmt"
	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/sys/unix"
)

func asset(t *testing.T, path string, data []byte) string {
	t.Helper()
	if os.WriteFile(path, data, 0700) != nil {
		t.Fatal("asset")
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func TestNativeAttemptFenceAndSecretFreeArgs(t *testing.T) {
	dir, e := os.MkdirTemp("/tmp", "runner-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	boot, _ := BootID()
	oracle := filepath.Join(dir, "oracle")
	binary := filepath.Join(dir, "vmm")
	// The installed v55 rejected the previous disk argument before boot. Model
	// that actual CLI requirement at dispatch, not just the Args return value.
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\nif [ \"$1\" = --disk ]; then\n" +
		"[ \"$2\" = 'path=" + filepath.Join(dir, "disk.raw") + ",image_type=raw' ] || exit 2\n" +
		"printf x >> '" + oracle + "'\nexit 0\nfi\nshift\ndone\nexit 2\n"
	digest := asset(t, binary, []byte(script))
	source := filepath.Join(dir, "source")
	sd := asset(t, source, []byte("fresh fixture image"))
	kernel := filepath.Join(dir, "kernel")
	kd := asset(t, kernel, []byte("fixture kernel"))
	s := Spec{Version: 1, Owner: "test", LaunchID: "launch", Identity: "identity", BootID: boot, Hypervisor: binary, HypervisorDigest: digest, Source: source, SourceDigest: sd, Kernel: kernel, KernelDigest: kd, DiskBytes: 1024, VCPUs: 4, MemoryBytes: 8 << 30, Facts: api.Allocation{Base: "ubuntu-24.04-arm64"}, Job: api.JobRef{JobDB: "https://jobdb.invalid/t", InstanceID: "instance", TenantID: "t", JobID: "job"}}
	if state.Atomic(filepath.Join(dir, "record.json"), s, nil) != nil {
		t.Fatal("record")
	}
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "handoff"), Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	delivered := make(chan error, 1)
	lease := []byte("opaque\x00\xff\n")
	go func() {
		c, e := l.AcceptUnix()
		if e != nil {
			delivered <- e
			return
		}
		defer c.Close()
		protocol.Write(c, protocol.Message{Kind: "deliver", Delivery: &protocol.Delivery{LaunchID: s.LaunchID, Identity: s.Identity, Job: s.Job, Lease: lease, Facts: s.Facts}})
		m, e := protocol.Read(c)
		if e == nil && m.Kind != "accepted" {
			e = ErrRunner
		}
		delivered <- e
	}()
	if e = Execute(dir, Digest(s)); e != nil {
		t.Fatal(e)
	}
	if e = <-delivered; e != nil {
		t.Fatal(e)
	}
	// A second runner invocation cannot reach the native executable, even after
	// the first runner has exited and its lock is available.
	if Execute(dir, Digest(s)) == nil {
		t.Fatal("start fence ignored")
	}
	b, _ := os.ReadFile(oracle)
	if string(b) != "x" {
		t.Fatal("native start repeated")
	}
	args := strings.Join(Args(dir, s), " ")
	if strings.Contains(args, string(lease)) || strings.Contains(args, "lease") {
		t.Fatal("lease in argv")
	}
	for _, name := range []string{"record.json", "status.json", "start.fence", "runner.lock"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if strings.Contains(string(b), string(lease)) {
			t.Fatal("lease persisted")
		}
	}
	status, e := ReadStatus(dir, s)
	if e != nil || status.Phase != "terminated" {
		t.Fatal("native termination unrecorded")
	}
}
func TestRunnerRejectsChangedBootAndIdentity(t *testing.T) {
	dir := t.TempDir()
	s := Spec{Version: 1, Owner: "owner", LaunchID: "launch", Identity: "identity", BootID: "old boot"}
	state.Atomic(filepath.Join(dir, "record.json"), s, nil)
	if Execute(dir, Digest(s)) == nil {
		t.Fatal("replayed after reboot")
	}
	if Execute(dir, "foreign-digest") == nil {
		t.Fatal("foreign runner adopted")
	}
}
func TestRuntimeCapSurvivesHandoffClosure(t *testing.T) {
	dir, e := os.MkdirTemp("/tmp", "cap-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	boot, _ := BootID()
	binary := filepath.Join(dir, "vmm")
	digest := asset(t, binary, []byte("#!/bin/sh\nexec /bin/sleep 30\n"))
	source := filepath.Join(dir, "source")
	sd := asset(t, source, []byte("fresh fixture image"))
	kernel := filepath.Join(dir, "kernel")
	kd := asset(t, kernel, []byte("fixture kernel"))
	s := Spec{Version: 1, Owner: "test", LaunchID: "cap", Identity: "identity", BootID: boot, Hypervisor: binary, HypervisorDigest: digest, Source: source, SourceDigest: sd, Kernel: kernel, KernelDigest: kd, DiskBytes: 1024, VCPUs: 4, MemoryBytes: 8 << 30, TimeoutSeconds: 1, Facts: api.Allocation{Base: "ubuntu-24.04-arm64"}, Job: api.JobRef{JobDB: "https://fixture.invalid/t", InstanceID: "i", TenantID: "t", JobID: "j"}}
	socket := filepath.Join(dir, "notify")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer notify.Close()
	t.Setenv("NOTIFY_SOCKET", socket)
	ctl := filepath.Join(dir, "systemctl")
	asset(t, ctl, []byte(fmt.Sprintf("#!/bin/sh\nprintf 'Description=pulse:identity\\nMainPID=%d\\nActiveState=active\\n'\n", os.Getpid())))
	asset(t, filepath.Join(dir, "busctl"), []byte("#!/bin/sh\nprintf 't 1000000\\n'\n"))
	s.Systemctl = ctl
	s.Unit = "fixture.service"
	state.Atomic(filepath.Join(dir, "record.json"), s, nil)
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "handoff"), Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	acknowledged := make(chan struct{})
	go func() {
		c, e := l.AcceptUnix()
		if e != nil {
			return
		}
		defer c.Close()
		protocol.Write(c, protocol.Message{Kind: "deliver", Delivery: &protocol.Delivery{LaunchID: s.LaunchID, Identity: s.Identity, Job: s.Job, Lease: []byte("synthetic"), Facts: s.Facts}})
		m, e := protocol.Read(c)
		if e == nil && m.Kind == "accepted" {
			close(acknowledged)
		}
	}()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Execute(dir, Digest(s)) }()
	select {
	case <-acknowledged:
		buf := make([]byte, 32)
		notify.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := notify.ReadFromUnix(buf)
		if err != nil || string(buf[:n]) != "READY=1" {
			t.Fatal("dispatch did not notify the supervisor")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch not acknowledged")
	}
	// The private provider handoff is now closed. Only the runner's cap can stop
	// this observable child; its configured native lifetime is thirty seconds.
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("runtime cap disappeared with observer")
	}
	if time.Since(start) < time.Second {
		t.Fatal("cap not measured from dispatch")
	}
	status, e := ReadStatus(dir, s)
	if e != nil || status.Phase != "terminated" {
		t.Fatal("capped native child not confirmed terminated")
	}
}

func TestUncappedUncertainGuestDeliveryNeverKillsForAge(t *testing.T) {
	dir, e := os.MkdirTemp("/tmp", "uncapped-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	boot, _ := BootID()
	fifo := filepath.Join(dir, "release")
	if unix.Mkfifo(fifo, 0600) != nil {
		t.Fatal("barrier")
	}
	binary := filepath.Join(dir, "vmm")
	digest := asset(t, binary, []byte("#!/bin/sh\nread value < '"+fifo+"'\n"))
	source := filepath.Join(dir, "source")
	sd := asset(t, source, []byte("fresh fixture image"))
	kernel := filepath.Join(dir, "kernel")
	kd := asset(t, kernel, []byte("fixture kernel"))
	s := Spec{Version: 1, Owner: "test", LaunchID: "uncapped", Identity: "identity", BootID: boot, Hypervisor: binary, HypervisorDigest: digest, Source: source, SourceDigest: sd, Kernel: kernel, KernelDigest: kd, DiskBytes: 1024, VCPUs: 4, MemoryBytes: 8 << 30, Facts: api.Allocation{Base: "ubuntu-24.04-arm64"}, Job: api.JobRef{JobDB: "https://fixture.invalid/t", InstanceID: "i", TenantID: "t", JobID: "j"}}
	state.Atomic(filepath.Join(dir, "record.json"), s, nil)
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "handoff"), Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	go func() {
		c, e := l.AcceptUnix()
		if e != nil {
			return
		}
		defer c.Close()
		protocol.Write(c, protocol.Message{Kind: "deliver", Delivery: &protocol.Delivery{LaunchID: s.LaunchID, Identity: s.Identity, Job: s.Job, Lease: []byte("synthetic"), Facts: s.Facts}})
		protocol.Read(c)
	}()
	delivered := make(chan struct{})
	connect := func(string, <-chan struct{}) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			m, e := protocol.Read(b)
			if e == nil && m.Delivery != nil && string(m.Delivery.Lease) == "synthetic" {
				close(delivered)
			}
		}()
		return a, nil
	}
	done := make(chan error, 1)
	go func() { done <- execute(dir, Digest(s), connect) }()
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("guest delivery")
	}
	// There was a successful write followed by lost guest status. A boot/delivery
	// timeout cannot prove the worker did not start or authorize terminating it.
	select {
	case <-done:
		t.Fatal("uncapped native process killed on uncertain delivery")
	default:
	}
	release, e := os.OpenFile(fifo, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	release.Write([]byte("finish\n"))
	release.Close()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uncapped process did not finish after explicit release")
	}
}

func TestPreparationRejectsChangedInitramfsBeforeNativeStart(t *testing.T) {
	dir := t.TempDir()
	boot, _ := BootID()
	oracle := filepath.Join(dir, "native-started")
	hypervisor := filepath.Join(dir, "vmm")
	hd := asset(t, hypervisor, []byte("#!/bin/sh\ntouch '"+oracle+"'\n"))
	source := filepath.Join(dir, "source")
	sd := asset(t, source, []byte("pristine source"))
	kernel := filepath.Join(dir, "kernel")
	kd := asset(t, kernel, []byte("kernel"))
	initramfs := filepath.Join(dir, "initramfs")
	rd := asset(t, initramfs, []byte("pinned ramdisk"))
	asset(t, initramfs, []byte("changed ramdisk"))
	s := Spec{Version: 1, Owner: "test", LaunchID: "prepare", Identity: "builder", BootID: boot, Hypervisor: hypervisor, HypervisorDigest: hd, Source: source, SourceDigest: sd, Kernel: kernel, KernelDigest: kd, Initramfs: initramfs, InitramfsDigest: rd, DiskBytes: 1024}
	if state.Atomic(filepath.Join(dir, "record.json"), s, nil) != nil || state.Atomic(filepath.Join(dir, "plan.json"), map[string]string{"Identity": s.Identity}, nil) != nil {
		t.Fatal("fixture")
	}
	if Prepare(dir, Digest(s)) == nil {
		t.Fatal("changed boot asset accepted")
	}
	progress, err := ReadStatus(dir, s)
	if err != nil || progress.Phase != "checking-inputs" {
		t.Fatal("unverified assets advanced builder progress")
	}
	for _, path := range []string{oracle, filepath.Join(dir, "disk.raw"), filepath.Join(dir, "proof.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("changed initramfs reached cloning, native dispatch or qualification")
		}
	}
}

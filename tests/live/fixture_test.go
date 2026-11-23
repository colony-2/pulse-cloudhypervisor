package live

import (
	"context"
	"encoding/json"
	"fmt"
	ch "github.com/colony-2/pulse-cloudhypervisor"
	api "github.com/colony-2/pulse-provider-api"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMissingFixtureAuthority(t *testing.T) {
	for _, path := range []string{"", "relative.json", filepath.Join(t.TempDir(), "absent")} {
		if _, _, err := loadFixture(path); err == nil {
			t.Fatal("missing fixture accepted")
		}
	}
	dir := t.TempDir()
	for _, data := range []string{`{"version":1,"lease":"synthetic-private-lease"}`, `{"version":2}`, `{`, `{}`, `{"version":1,"config":"synthetic-private-lease"} {}`, `{"version":1,"identity":"\u0000synthetic-private-lease"}`} {
		path := filepath.Join(dir, "fixture.json")
		if os.WriteFile(path, []byte(data), 0600) != nil {
			t.Fatal("fixture")
		}
		if _, _, err := loadFixture(path); err == nil || err.Error() != errFixture.Error() {
			t.Fatal("invalid fixture accepted or unsafe diagnostics")
		}
	}
}

func TestGuestFaultsDoNotRequireOrGrantReboot(t *testing.T) {
	f := fixture{Faults: []string{"controller-stop", "runner-kill"}}
	if f.guestFaultAuthority() != nil {
		t.Fatal("missing separate reboot grant blocked guest fault tests")
	}
	f.Faults = []string{"host-reboot"}
	if f.guestFaultAuthority() != errFixture {
		t.Fatal("legacy reboot declaration granted controller/runner faults")
	}
	for _, faults := range [][]string{nil, {"controller-stop"}, {"runner-kill"}, {"controller-stop", "controller-stop"}, {"controller-stop", "runner-kill-extra"}} {
		if (fixture{Faults: faults}).guestFaultAuthority() != errFixture {
			t.Fatal("incomplete guest fault authority accepted")
		}
	}
}

func TestFixtureConnectionRequiresProtectedSameUserSocket(t *testing.T) {
	// Short path avoids platform Unix socket length limits in Go test directories.
	dir, err := os.MkdirTemp("/tmp", "live-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "control")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f := fixture{ControlSocket: path}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if os.Chmod(path, 0666) != nil {
		t.Fatal("permissions")
	}
	if c, e := f.connect(ctx); e == nil {
		c.Close()
		t.Fatal("public fixture channel accepted")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("permissions")
	}
	accepted := make(chan struct{})
	go func() {
		c, e := l.AcceptUnix()
		if e == nil {
			c.Close()
		}
		close(accepted)
	}()
	c, err := f.connect(ctx)
	if err != nil {
		t.Fatal("same-user protected fixture channel rejected")
	}
	c.Close()
	<-accepted
}

func TestConcurrentCapacity(t *testing.T) {
	count := 2
	c := ch.Config{Count: &count, VCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 32 << 30,
		HostCPUMillis: 8000, HostMemoryBytes: 16 << 30, HostDiskBytes: 96 << 30,
		Network: make([]ch.Network, 2)}
	if !concurrentCapacity(c, 8) {
		t.Fatal("fitting profile rejected")
	}
	for _, kind := range []string{"cpus", "cpu_budget", "memory", "disk", "count", "network"} {
		t.Run(kind, func(t *testing.T) {
			bad := c
			cpus := 8
			switch kind {
			case "cpus":
				cpus--
			case "cpu_budget":
				bad.HostCPUMillis--
			case "memory":
				bad.HostMemoryBytes--
			case "disk":
				bad.HostDiskBytes--
			case "count":
				bad.Count = nil
			case "network":
				bad.Network = bad.Network[:1]
			}
			if concurrentCapacity(bad, cpus) {
				t.Fatal("insufficient capacity accepted")
			}
		})
	}
}

func TestFixtureRejectsPeerAuthentication(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "live-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if os.Chmod(path, 0600) != nil {
		t.Fatal("permissions")
	}
	// Inject a peer-auth rejection to exercise the fixture boundary on any UID.
	// connect always supplies the real SO_PEERCRED check in production.
	f := fixture{ControlSocket: path}
	called := false
	conn, err := f.connectAuthenticated(context.Background(), func(*net.UnixConn) error {
		called = true
		return fmt.Errorf("synthetic-private-lease")
	})
	if conn != nil || err != errFixture || !called {
		t.Fatal("peer rejection was bypassed or exposed")
	}
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal("accept rejected connection")
	}
	defer peer.Close()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatal("rejected connection remains open")
	}
}

// This server supplies synthetic wire data to exercise the private client only;
// real JobDB/c2j integration runs in the nested tests/fixture module.
func TestFixtureProtocol(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ch-client-")
	if err != nil {
		t.Fatal("directory")
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control")
	listener, err := net.Listen("unix", socket)
	if err != nil || os.Chmod(socket, 0600) != nil {
		t.Fatal("socket")
	}
	f := fixture{Identity: "test-fixture", ControlSocket: socket}
	var mu sync.Mutex
	mode := "valid"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := mode
		mu.Unlock()
		if r.Header.Get("X-Pulse-Fixture") != f.Identity {
			http.Error(w, "denied", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if current != "valid" {
			switch current {
			case "redirect":
				http.Redirect(w, r, "http://unexpected.invalid/private", 302)
			case "large":
				io.WriteString(w, strings.Repeat(" ", 1<<20)+"{}")
			case "bad":
				io.WriteString(w, `{"lease":"synthetic-private-lease","unexpected":true}`)
			case "trailing":
				io.WriteString(w, `{"ready":true} {}`)
			case "empty":
				io.WriteString(w, `{"ready":false}`)
			case "cache":
				w.Header().Del("Cache-Control")
				io.WriteString(w, `{"ready":true}`)
			}
			return
		}
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]bool{"ready": true})
		case "/launch":
			if r.URL.Query().Get("name") != "case-one" || r.URL.Query().Get("case") != "valid" {
				http.Error(w, "denied", 400)
				return
			}
			json.NewEncoder(w).Encode(fixtureAssignment{Job: api.JobRef{JobDB: "http://fixture.invalid/tenant", InstanceID: "instance", TenantID: "tenant", JobID: "job"}, Lease: "synthetic-private-lease"})
		case "/status":
			json.NewEncoder(w).Encode(fixtureStatus{Job: api.JobRef{JobDB: "http://fixture.invalid/tenant", InstanceID: "instance", TenantID: "tenant", JobID: "job"}, State: "ACTIVE", InitialExpiry: time.Unix(10, 0), LatestExpiry: time.Unix(20, 0), Acquisitions: 1, Renewals: 2})
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if f.ready(ctx) != nil {
		t.Fatal("health handshake")
	}
	a, err := f.assignment(ctx, "case-one", "valid")
	if err != nil || a.Job.InstanceID != "instance" || a.Job.TenantID != "tenant" || a.Job.JobID != "job" || a.Job.JobDB != "http://fixture.invalid/tenant" || a.Lease != "synthetic-private-lease" {
		t.Fatal("assignment altered")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", a, a), "synthetic-private-lease") {
		t.Fatal("assignment formatting exposed credential")
	}
	if s, err := f.observe(ctx, "case-one"); err != nil || s.Renewals != 2 || s.OutputOK {
		t.Fatal("observation altered")
	}
	for _, current := range []string{"redirect", "large", "bad", "trailing", "empty", "cache"} {
		mu.Lock()
		mode = current
		mu.Unlock()
		if err := f.ready(ctx); err != errFixture {
			t.Fatal("invalid health response accepted or diagnostic exposed")
		}
		if a, err := f.assignment(ctx, "case-one", "valid"); err != errFixture || a.Lease != "" {
			t.Fatal("invalid assignment response accepted or exposed")
		}
	}
	for _, name := range []string{"", "../escape", "case?token=private"} {
		if _, err := f.assignment(ctx, name, "valid"); err != errFixture {
			t.Fatal("invalid case name accepted")
		}
		if _, err := f.observe(ctx, name); err != errFixture {
			t.Fatal("invalid observation name accepted")
		}
	}
	if _, err := f.assignment(ctx, "case-one", "unknown"); err != errFixture {
		t.Fatal("unknown variant accepted")
	}
}

func TestFixtureJobIdentityValidation(t *testing.T) {
	good := api.JobRef{JobDB: "http://fixture.invalid/tenant", InstanceID: "instance", TenantID: "tenant", JobID: "job"}
	if !validFixtureJob(good) {
		t.Fatal("valid reference rejected")
	}
	for _, field := range []string{"endpoint", "credentials", "instance", "tenant", "job", "control"} {
		bad := good
		switch field {
		case "endpoint":
			bad.JobDB = "file:///private"
		case "credentials":
			bad.JobDB = "http://private:secret@fixture.invalid/tenant"
		case "instance":
			bad.InstanceID = ""
		case "tenant":
			bad.TenantID = ""
		case "job":
			bad.JobID = ""
		case "control":
			bad.JobID = "private\x01"
		}
		if validFixtureJob(bad) {
			t.Fatal("incomplete or unsafe reference accepted")
		}
	}
}

// Run the production verifier boundaries against synthetic records. These
// Docker- and KVM-independent checks belong to the local Go receipt.
func verifyBoundary(t *testing.T, name string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test location unavailable")
	}
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "verify_test.py")
	if exec.Command("python3", script, "VerificationTests."+name).Run() != nil {
		t.Fatalf("verification boundary %s failed", name)
	}
}

func TestS08(t *testing.T) { verifyBoundary(t, "test_owned_unit_record_and_ambiguity") }
func TestS10(t *testing.T) {
	verifyBoundary(t, "test_missing_el2_preserves_independent_dispatch_but_fails_verification")
}

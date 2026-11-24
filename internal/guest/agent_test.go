package guest

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	api "github.com/colony-2/pulse-provider-api"
)

func fixture(t *testing.T) (Config, protocol.Delivery, string) {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "c2j")
	// A real observable child records argv/environment separately from stdin,
	// produces more than pipe/queue capacity, and marks successful completion.
	body := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > '" + root + "/argv'\nenv > '" + root + "/env'\ncat > '" + root + "/stdin'\nprintf x >> '" + root + "/starts'\nhead -c 1048576 /dev/zero\nhead -c 1048576 /dev/zero >&2\nprintf done > '" + root + "/completed'\n"
	if os.WriteFile(script, []byte(body), 0700) != nil {
		t.Fatal("child fixture")
	}
	cfg := Config{LaunchID: "launch", Identity: "identity", IntentDir: filepath.Join(root, "intent"), C2J: script, WorkDir: root}
	d := protocol.Delivery{LaunchID: cfg.LaunchID, Identity: cfg.Identity, Job: api.JobRef{JobDB: "https://jobdb.invalid/t", InstanceID: "instance", TenantID: "t", JobID: "job"}, Lease: []byte("synthetic-private\x00\xff\n "), Facts: api.Allocation{Base: "ubuntu-24.04-arm64", CPUMillis: 4000, MemoryBytes: 7 << 30, Capabilities: []string{"docker"}}, Platform: "linux/arm64"}
	return cfg, d, root
}
func session(t *testing.T, cfg Config, d protocol.Delivery, disconnect bool) error {
	t.Helper()
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), guest, cfg) }()
	if protocol.Write(host, protocol.Message{Kind: "deliver", Delivery: &d}) != nil {
		t.Fatal("deliver")
	}
	if disconnect {
		host.Close()
	} else {
		go func() {
			defer host.Close()
			for {
				m, e := protocol.Read(host)
				if e != nil || m.Kind == "exit" {
					return
				}
			}
		}()
	}
	select {
	case e := <-done:
		return e
	case <-time.After(5 * time.Second):
		host.Close()
		t.Fatal("child blocked after listener loss")
		return nil
	}
}
func TestExactSuppliedLeaseAndGuestIntentFence(t *testing.T) {
	cfg, d, root := fixture(t)
	d.Facts.ScratchBytes = 1001
	if e := session(t, cfg, d, false); e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(filepath.Join(root, "stdin"))
	if !bytes.Equal(got, d.Lease) {
		t.Fatal("lease bytes changed")
	}
	args, _ := os.ReadFile(filepath.Join(root, "argv"))
	if !strings.Contains(string(args), "--execution-memory\n7340032Ki\n") ||
		!strings.Contains(string(args), "--execution-ephemeral-storage\n1.001k\n") {
		t.Fatal("allocation byte quantities lost their exact c2j units")
	}
	env, _ := os.ReadFile(filepath.Join(root, "env"))
	if bytes.Contains(args, d.Lease) || bytes.Contains(env, d.Lease) || !strings.Contains(string(args), "with-lease\n") || !strings.Contains(string(args), "--lease-file\n-\n") || !strings.Contains(string(args), d.Facts.Base) || strings.Contains(string(args), "claim") {
		t.Fatal("unsafe c2j invocation")
	}
	// Restarting the agent and delivering the same assignment cannot start twice.
	if session(t, cfg, d, true) == nil {
		t.Fatal("duplicate worker accepted")
	}
	starts, _ := os.ReadFile(filepath.Join(root, "starts"))
	if string(starts) != "x" {
		t.Fatal("worker replayed")
	}
	intent, _ := os.ReadFile(filepath.Join(cfg.IntentDir, "worker-intent"))
	if bytes.Contains(intent, d.Lease) {
		t.Fatal("credential persisted in fence")
	}
}
func TestGuestDrainsAfterDisconnect(t *testing.T) {
	cfg, d, root := fixture(t)
	if e := session(t, cfg, d, true); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "completed")); string(b) != "done" {
		t.Fatal("high output blocked worker")
	}
}
func TestGuestRejectsPartialAndWrongIdentityBeforeIntent(t *testing.T) {
	for _, kind := range []string{"identity", "partial", "wrong-kind"} {
		t.Run(kind, func(t *testing.T) {
			cfg, d, root := fixture(t)
			a, b := net.Pipe()
			done := make(chan error, 1)
			go func() { done <- Serve(context.Background(), b, cfg) }()
			switch kind {
			case "identity":
				d.Identity = "foreign"
				protocol.Write(a, protocol.Message{Kind: "deliver", Delivery: &d})
			case "wrong-kind":
				protocol.Write(a, protocol.Message{Kind: "status"})
			case "partial":
				a.Write([]byte{0, 0, 0, 4, '{'})
			}
			a.Close()
			if <-done == nil {
				t.Fatal("invalid message executed")
			}
			if _, e := os.Stat(filepath.Join(root, "starts")); !os.IsNotExist(e) {
				t.Fatal("child started on invalid delivery")
			}
			if _, e := os.Stat(filepath.Join(cfg.IntentDir, "worker-intent")); !os.IsNotExist(e) {
				t.Fatal("invalid delivery committed intent")
			}
		})
	}
}
func TestConcurrentGuestDeliveriesStartOnce(t *testing.T) {
	cfg, d, root := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); session(t, cfg, d, true) }()
	}
	wg.Wait()
	starts, _ := os.ReadFile(filepath.Join(root, "starts"))
	if string(starts) != "x" {
		t.Fatal("concurrent guest starts")
	}
}

func TestNestedLaunchModeMustMatchDeliveredFacts(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg, d, root := fixture(t)
		cfg.NestedKVM = enabled
		if !enabled {
			d.Facts.Capabilities = append(d.Facts.Capabilities, "kvm")
		}
		if !errors.Is(session(t, cfg, d, false), ErrAgent) {
			t.Fatal("mismatched KVM handoff accepted")
		}
		if _, err := os.Stat(filepath.Join(root, "starts")); !os.IsNotExist(err) {
			t.Fatal("worker ran with inaccurate allocation facts")
		}
	}
}

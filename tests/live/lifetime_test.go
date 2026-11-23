//go:build live

package live

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

func (e *executionFixture) waitForWorker(t *testing.T, out *guestOutput) {
	t.Helper()
	for {
		out.mu.Lock()
		started := out.started
		out.mu.Unlock()
		if started {
			return
		}
		select {
		case <-e.ctx.Done():
			t.Fatal("guest c2j start deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *executionFixture) reopen(t *testing.T) {
	t.Helper()
	if e.provider.Close() != nil {
		t.Fatal("close provider")
	}
	p, err := ch.New(e.config)
	if err != nil {
		t.Fatal("reopen provider")
	}
	t.Cleanup(func() { p.Close() })
	e.provider = p
}

func TestNativeControllerClose(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "controller-close", "valid", "lifetime")
	out := new(guestOutput)
	e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	e.waitForWorker(t, out)
	dir := launchDirectory(e.config.StateDir, l.LaunchID)
	before, err := runner.ReadSpec(dir)
	if err != nil || before.Job != l.Job || before.TimeoutSeconds != 0 {
		t.Fatal("owned uncapped launch record")
	}
	fence, err := os.Stat(filepath.Join(dir, "start.fence"))
	if err != nil {
		t.Fatal("durable start fence missing")
	}
	e.reopen(t)
	page, err := e.provider.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
		t.Fatal("active launch missing after controller close")
	}
	after, err := runner.ReadSpec(dir)
	fenceAfter, statErr := os.Stat(filepath.Join(dir, "start.fence"))
	if err != nil || statErr != nil || runner.Digest(before) != runner.Digest(after) || !os.SameFile(fence, fenceAfter) {
		t.Fatal("reopened launch or durable start fence changed identity")
	}
	e.waitForCleanup(t, l)
	status, err := e.observe(e.ctx, l.LaunchID)
	if err != nil || status.Job != l.Job || status.Acquisitions != 1 || status.ClaimAttempts != 0 ||
		!status.RenewedBeyondInitial || !status.OutputOK || status.Outcome != "success" {
		t.Fatal("uncapped job failed to renew and finish across provider closure")
	}
}

func TestNativeCompletionCleanup(t *testing.T) {
	e := openExecution(t)
	foreign := cleanupCanary(t, e.config.StateDir)
	l, _ := e.launch(t, "cleanup-completion", "valid", "docker")
	out := new(guestOutput)
	sub := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	outcome := e.finish(t, l, sub, out, true)
	if !outcome.OutputOK {
		t.Fatal("expected guest completion before cleanup")
	}
	e.assertTerminalCleanup(t, l, foreign)
}

func cleanupCanary(t *testing.T, stateDir string) string {
	t.Helper()
	f, err := os.CreateTemp(filepath.Dir(stateDir), "unrelated-")
	if err != nil {
		t.Fatal("create foreign canary")
	}
	if _, err := f.WriteString("independent fixture canary"); err != nil {
		f.Close()
		t.Fatal("write foreign canary")
	}
	if err := f.Close(); err != nil {
		t.Fatal("seal foreign canary")
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func (e *executionFixture) assertTerminalCleanup(t *testing.T, l api.Launch, foreign string) {
	t.Helper()
	dir := launchDirectory(e.config.StateDir, l.LaunchID)
	var fence struct {
		Version                   int
		LaunchID, Owner, Identity string
	}
	if state.Read(filepath.Join(dir, "terminal.json"), &fence) != nil || fence.Version != 1 ||
		fence.LaunchID != l.LaunchID || fence.Owner != e.config.Owner || len(fence.Identity) != 32 {
		t.Fatal("terminal fence lost launch ownership")
	}
	for _, name := range []string{"disk.raw", "api", "api.lock", "vsock", "handoff", "record.json", "status.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("owned runtime resource remains after reconciliation")
		}
	}
	got, err := os.ReadFile(foreign)
	if err != nil || !bytes.Equal([]byte("independent fixture canary"), got) {
		t.Fatal("foreign canary changed during cleanup")
	}
}

func TestS19(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "guest-lost-ack", "valid", "ack-lifetime")
	if e.provider.Close() != nil {
		t.Fatal("release owner for acknowledgement fault")
	}
	// The managed fixture compiles this test-only client from the same checkout.
	cfg := e.config
	cfg.Runner = filepath.Join(filepath.Dir(e.ConfigPath), "ack-loss-runner")
	testOwner, err := ch.New(cfg)
	if err != nil {
		t.Fatal("open controlled acknowledgement fault owner")
	}
	e.provider = testOwner
	t.Cleanup(func() { testOwner.Close() })
	e.submit(t, l, api.SubmitOptions{})
	dir := launchDirectory(cfg.StateDir, l.LaunchID)
	spec, err := runner.ReadSpec(dir)
	if err != nil || spec.Job != l.Job || spec.TimeoutSeconds != 0 {
		t.Fatal("identify uncapped guest")
	}
	marker := filepath.Join(dir, "ack-lost.json")
	var identity string
	for {
		if state.Read(marker, &identity) == nil {
			break
		}
		select {
		case <-e.ctx.Done():
			t.Fatal("actual guest acknowledgement was never observed at the fault point")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if identity != spec.Identity || os.Remove(marker) != nil {
		t.Fatal("acknowledgement identity mismatch")
	}
	// The production boot grace is one minute. This uncapped worker has already
	// received the lease and must remain alive beyond that grace after read loss.
	stillActiveAt := time.Now().Add(time.Minute)
	fence, err := os.Stat(filepath.Join(dir, "start.fence"))
	if err != nil {
		t.Fatal("missing durable start fence")
	}
	e.reopen(t)
	repeat, _, err := e.provider.Submit(e.ctx, []api.Launch{l}, api.SubmitOptions{})
	if err != nil || len(repeat) != 1 || repeat[0].Status != api.Rejected {
		t.Fatal("uncertain launch permitted replay")
	}
	select {
	case <-e.ctx.Done():
		t.Fatal("uncertainty observation timed out")
	case <-time.After(time.Until(stillActiveAt)):
	}
	if err := e.provider.Reconcile(e.ctx); err != nil {
		t.Fatal("reconcile uncertain workload")
	}
	page, err := e.provider.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
	after, statErr := os.Stat(filepath.Join(dir, "start.fence"))
	if err != nil || statErr != nil || !os.SameFile(fence, after) || len(page.Items) != 1 || page.Items[0].Job != l.Job {
		t.Fatal("possibly started uncapped worker lost its reservation or fence")
	}
	e.waitForCleanup(t, l)
	outcome, err := e.observe(e.ctx, l.LaunchID)
	if err != nil || outcome.Job != l.Job || outcome.Acquisitions != 1 || outcome.ClaimAttempts != 0 ||
		!outcome.RenewedBeyondInitial || !outcome.OutputOK || outcome.Outcome != "success" {
		t.Fatal("uncertain worker failed to renew and finish its original lease")
	}
}

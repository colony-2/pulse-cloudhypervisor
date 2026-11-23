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
	foreign := filepath.Join(filepath.Dir(e.config.StateDir), "foreign-state")
	marker := []byte("independent fixture canary")
	if err := os.WriteFile(foreign, marker, 0600); err != nil {
		t.Fatal("create foreign canary")
	}
	t.Cleanup(func() { os.Remove(foreign) })
	l, _ := e.launch(t, "cleanup-completion", "valid", "docker")
	out := new(guestOutput)
	sub := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	outcome := e.finish(t, l, sub, out, true)
	if !outcome.OutputOK {
		t.Fatal("expected guest completion before cleanup")
	}
	if _, err := os.Lstat(launchDirectory(e.config.StateDir, l.LaunchID)); !os.IsNotExist(err) {
		t.Fatal("job resources remain after terminal reconciliation")
	}
	got, err := os.ReadFile(foreign)
	if err != nil || !bytes.Equal(marker, got) {
		t.Fatal("foreign canary changed during cleanup")
	}
}

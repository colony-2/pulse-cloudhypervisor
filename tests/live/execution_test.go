//go:build live

package live

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	api "github.com/colony-2/pulse-provider-api"
)

// Each test uses its own names in this invocation's disposable JobDB. Keep
// leases and guest output in memory. Report only the observed milestone.
type executionFixture struct {
	fixture
	config   ch.Config
	provider *ch.Runtime
	ctx      context.Context
}

// A failed assertion can leave a finished VM reserved. After the test's
// controllers close, let the provider reconcile only proven terminations
// before the next case exercises this same two-slot fixture.
func reconcileAfterFailure(t *testing.T, c ch.Config) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := ch.New(c)
		if err != nil {
			t.Error("could not reopen the fixture owner after test failure")
			return
		}
		defer p.Close()
		if err := p.Reconcile(ctx); err != nil {
			t.Error("failed test left an uncertain native reservation")
		}
	})
}

func openExecution(t *testing.T) *executionFixture {
	t.Helper()
	f, c, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil {
		t.Fatal("private native fixture is required")
	}
	reconcileAfterFailure(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if f.ready(ctx) != nil {
		t.Fatal("guest fixture unavailable")
	}
	p, err := ch.New(c)
	if err != nil {
		t.Fatal("open native provider")
	}
	t.Cleanup(func() { p.Close() })
	caps, err := p.GetCapabilities(ctx)
	if err != nil || len(caps.Bases) != 1 || caps.Bases[0].Name != c.Bases[0].Name {
		t.Fatal("prepared base unavailable before lease delivery")
	}
	return &executionFixture{f, c, p, ctx}
}

func (e *executionFixture) launch(t *testing.T, name, variant, profile string) (api.Launch, fixtureStatus) {
	t.Helper()
	a, err := e.assignmentProfile(e.ctx, name, variant, profile)
	if err != nil {
		t.Fatal("obtain private assignment")
	}
	before, err := e.observe(e.ctx, name)
	if err != nil || before.Acquisitions != 1 || before.ClaimAttempts != 0 {
		t.Fatal("initial fixture state")
	}
	return api.Launch{LaunchID: name, Job: a.Job, Lease: a.Lease, Allocation: api.Allocation{Base: "ubuntu*-arm64", Capabilities: []string{"docker"}}}, before
}

type guestOutput struct {
	mu                           sync.Mutex
	started, succeeded, rejected bool
}

func (o *guestOutput) listen(ctx context.Context, id string) api.SubmitOptions {
	return api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx, OnLog: func(_ context.Context, l api.LaunchLog) error {
		if l.LaunchID == id && l.Source == api.LogSourceLaunch {
			o.mu.Lock()
			defer o.mu.Unlock()
			switch string(l.Data) {
			case "guest worker intent committed":
				o.started = true
			case "guest worker exited successfully":
				o.succeeded = true
			case "guest worker exited unsuccessfully":
				o.rejected = true
			}
		}
		return nil
	}}}
}

func (e *executionFixture) submit(t *testing.T, l api.Launch, opts api.SubmitOptions) api.LogSubscription {
	t.Helper()
	rs, sub, err := e.provider.Submit(e.ctx, []api.Launch{l}, opts)
	if err != nil || len(rs) != 1 || rs[0].LaunchID != l.LaunchID || rs[0].Status != api.Accepted {
		t.Fatal("native dispatch was not accepted")
	}
	if sub != nil {
		t.Cleanup(sub.Stop)
	}
	return sub
}

func (e *executionFixture) waitForCleanup(t *testing.T, l api.Launch) {
	t.Helper()
	for {
		page, err := e.provider.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
		if err != nil {
			t.Fatal("read active launch")
		}
		if len(page.Items) == 0 {
			// A VM can exit between two observations. Reconcile after seeing it
			// leave inventory, then verify that its reservation was released.
			if e.provider.Reconcile(e.ctx) != nil {
				t.Fatal("reconcile owned launch")
			}
			record := filepath.Join(launchDirectory(e.config.StateDir, l.LaunchID), "record.json")
			if _, err := os.Lstat(record); !os.IsNotExist(err) {
				t.Fatal("finished guest retained its reservation")
			}
			return
		}
		if len(page.Items) != 1 || page.Items[0].Job != l.Job {
			t.Fatal("active launch lost job correlation")
		}
		select {
		case <-e.ctx.Done():
			t.Fatal("owned job still active at deadline")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (e *executionFixture) finish(t *testing.T, l api.Launch, sub api.LogSubscription, out *guestOutput, valid bool) fixtureStatus {
	t.Helper()
	result, err := sub.Wait(e.ctx)
	if err != nil || result.Reason != api.ListenerSourcesClosed {
		t.Fatal("native guest output did not close cleanly")
	}
	out.mu.Lock()
	ok := out.started && out.succeeded == valid && out.rejected != valid
	out.mu.Unlock()
	if !ok {
		t.Fatal("guest c2j start and exit status were not observed")
	}
	e.waitForCleanup(t, l)
	s, err := e.observe(e.ctx, l.LaunchID)
	if err != nil || s.Acquisitions != 1 || s.ClaimAttempts != 0 {
		t.Fatal("authoritative fixture accounting")
	}
	return s
}

func TestNativeInvalidLeases(t *testing.T) {
	e := openExecution(t)
	for _, variant := range []string{"expired", "mismatched", "completed"} {
		t.Run(variant, func(t *testing.T) {
			l, before := e.launch(t, "invalid-"+variant, variant, "renewal")
			out := new(guestOutput)
			sub := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
			after := e.finish(t, l, sub, out, false)
			if after.Job != before.Job || after.OutputOK || after.Chapters != before.Chapters || after.Renewals != before.Renewals || after.Outcome != before.Outcome {
				t.Fatal("rejected assignment changed the originally leased job")
			}
			if (variant == "mismatched") != (l.Job.JobID != after.Job.JobID) {
				t.Fatal("fixture did not exercise requested identity mismatch")
			}
		})
	}
}

func TestNativeGuestDocker(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "guest-docker", "valid", "docker")
	out := new(guestOutput)
	sub := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	s := e.finish(t, l, sub, out, true)
	if s.Job != l.Job || s.State != "COMPLETED" || s.Outcome != "success" || !s.OutputOK {
		t.Fatal("guest-local Docker workload failed")
	}
}

// TestS13 compares the allocation delivered to the guest command against both
// the selected usable profile and independent observations inside that VM.
func TestS13(t *testing.T) {
	e := openExecution(t)
	base := e.config.Bases[0]
	if base.UsableCPUMillis != 1000 || base.UsableMemoryBytes != 512<<20 || base.Name != "ubuntu-24.04-arm64" {
		t.Fatal("unexpected prepared test profile")
	}
	l, before := e.launch(t, "guest-allocation-renewal", "valid", "facts")
	out := new(guestOutput)
	sub := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	after := e.finish(t, l, sub, out, true)
	if after.Job != l.Job || !after.OutputOK || after.Outcome != "success" || after.State != "COMPLETED" ||
		after.Renewals == 0 || !after.RenewedBeyondInitial || !after.LatestExpiry.After(before.InitialExpiry) ||
		!time.Now().After(before.InitialExpiry) {
		t.Fatal("guest facts or renewal beyond the initial supplied lease were not verified")
	}
}

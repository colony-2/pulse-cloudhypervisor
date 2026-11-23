//go:build live

package live

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	api "github.com/colony-2/pulse-provider-api"
)

func launchDirectory(state, name string) string {
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(state, "jobs", fmt.Sprintf("%x", digest[:12]))
}

type isolationEvidence struct {
	distinct      bool
	privateWrites bool
}

var isolationRun struct {
	sync.Once
	result *isolationEvidence
}

// Both assertions examine the same pair of live guests. Running either test
// alone collects the complete observation with fresh, fixture-owned leases.
func concurrentGuests(t *testing.T) *isolationEvidence {
	t.Helper()
	isolationRun.Do(func() { isolationRun.result = observeConcurrentGuests(t) })
	if isolationRun.result == nil {
		t.Fatal("concurrent guest observation unavailable")
	}
	return isolationRun.result
}

func observeConcurrentGuests(t *testing.T) *isolationEvidence {
	t.Helper()
	e := openExecution(t)
	if !preparedDisk(e.config, e.config.Bases[0]) {
		t.Fatal("measure prepared source before private writes")
	}
	var launches [2]api.Launch
	var outputs [2]guestOutput
	var observations [2]api.LogSubscription
	var specs [2]runner.Spec
	var disks [2]os.FileInfo
	for i, name := range []string{"isolate-a", "isolate-b"} {
		launches[i], _ = e.launch(t, name, "valid", "peer-isolation")
		observations[i] = e.submit(t, launches[i], outputs[i].listen(e.ctx, name))
		dir := launchDirectory(e.config.StateDir, name)
		spec, err := runner.ReadSpec(dir)
		if err != nil || spec.Job != launches[i].Job || spec.Owner != e.config.Owner {
			t.Fatal("native launch ownership")
		}
		specs[i] = spec
		disks[i], err = os.Stat(filepath.Join(dir, "disk.raw"))
		if err != nil {
			t.Fatal("private disk missing while launch is active")
		}
	}
	a, b := specs[0], specs[1]
	for i := range outputs {
		e.waitForWorker(t, &outputs[i])
	}
	for i := range outputs {
		out := &outputs[i]
		out.mu.Lock()
		active := out.started && !out.succeeded && !out.rejected
		out.mu.Unlock()
		if !active {
			t.Fatal("both guest workers must overlap")
		}
	}
	page, err := e.provider.List(e.ctx, api.ListRequest{})
	result := &isolationEvidence{
		distinct: err == nil && len(page.Items) == 2 && a.Identity != b.Identity && a.TAP != b.TAP &&
			a.CID != b.CID && a.MAC != b.MAC && a.Address != b.Address && !os.SameFile(disks[0], disks[1]),
		privateWrites: a.Source == b.Source && a.SourceDigest == b.SourceDigest && images.Verify(a.Source, a.SourceDigest) == nil,
	}
	for i, l := range launches {
		status := e.finish(t, l, observations[i], &outputs[i], true)
		id := status.Identity
		s := specs[i]
		result.distinct = result.distinct && id != nil && id.Machine == s.Identity &&
			id.Launch == s.LaunchID && id.Address == s.Address && id.MAC == s.MAC && id.CID == s.CID
		// The peer-isolation command successfully verifies its own marker and
		// the absence of its peer's before emitting the checked guest identity.
		result.privateWrites = result.privateWrites && status.OutputOK && status.Outcome == "success"
	}
	result.privateWrites = result.privateWrites && images.Verify(a.Source, a.SourceDigest) == nil
	return result
}

func TestS15(t *testing.T) {
	if !concurrentGuests(t).distinct {
		t.Fatal("overlapping guests or writable disks lack distinct identities")
	}
}

func TestS16(t *testing.T) {
	if !concurrentGuests(t).privateWrites {
		t.Fatal("peer marker isolation or immutable prepared source failed")
	}
}

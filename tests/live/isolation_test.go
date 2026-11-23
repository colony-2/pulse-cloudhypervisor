//go:build live

package live

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	api "github.com/colony-2/pulse-provider-api"
)

func launchDirectory(state, name string) string {
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(state, "jobs", fmt.Sprintf("%x", digest[:12]))
}

func TestNativeConcurrentIsolation(t *testing.T) {
	e := openExecution(t)
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
	page, err := e.provider.List(e.ctx, api.ListRequest{})
	if err != nil || len(page.Items) != 2 || a.Source != b.Source || a.SourceDigest != b.SourceDigest ||
		a.Identity == b.Identity || a.TAP == b.TAP || a.CID == b.CID || a.MAC == b.MAC || a.Address == b.Address ||
		os.SameFile(disks[0], disks[1]) || images.Verify(a.Source, a.SourceDigest) != nil {
		t.Fatal("overlapping guests or prepared source lack independent identities")
	}
	for i, l := range launches {
		status := e.finish(t, l, observations[i], &outputs[i], true)
		id := status.Identity
		s := specs[i]
		if !status.OutputOK || id == nil || id.Machine != s.Identity || id.Launch != s.LaunchID ||
			id.Address != s.Address || id.MAC != s.MAC || id.CID != s.CID {
			t.Fatal("guest-observed isolation identity or marker mismatch")
		}
	}
	if images.Verify(a.Source, a.SourceDigest) != nil {
		t.Fatal("prepared source changed after concurrent writes")
	}
}

package cloudhypervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

func TestPreparationProofAndImmutablePromotion(t *testing.T) {
	p, _ := testRuntime(t)
	b := p.config.Bases[0]
	root := filepath.Join(p.config.StateDir, "sources")
	if state.PrivateDir(root) != nil {
		t.Fatal("sources")
	}
	builder := filepath.Join(root, "build-test")
	state.PrivateDir(builder)
	image := []byte("fresh stopped prepared source, without worker intent")
	h := sha256.Sum256(image)
	digest := hex.EncodeToString(h[:])
	os.WriteFile(filepath.Join(builder, "disk.raw"), image, 0600)
	proof := guest.Proof{Ubuntu: "24.04", Architecture: b.Architecture, Recipe: b.Recipe, Tools: b.Tools, Docker: true, DockerImage: b.DockerFixtureDigest, CPUMillis: 4000, MemoryBytes: 7 << 30}
	bad := proof
	bad.Docker = false
	if p.promoteSource(b, builder, digest, bad) == nil {
		t.Fatal("unverified Docker advertised")
	}
	bad = proof
	bad.MemoryBytes = 1
	if p.promoteSource(b, builder, digest, bad) == nil {
		t.Fatal("nominal memory advertised as usable")
	}
	if p.promoteSource(b, builder, strings.Repeat("0", 64), proof) == nil {
		t.Fatal("digest mismatch published")
	}
	if p.promoteSource(b, builder, digest, proof) != nil {
		t.Fatal("valid stopped source not promoted")
	}
	saved, e := p.sourceProof(b)
	if e != nil || !p.sourceReady(b) {
		t.Fatal("source proof unavailable")
	}
	info, _ := os.Stat(saved.Path)
	if info.Mode().Perm() != 0400 {
		t.Fatal("source writable")
	}
	// A changed recipe/tool identity selects another source without modifying
	// previously accepted jobs or reinterpreting the earlier source.
	newer := b
	newer.Tools = map[string]string{}
	for k, v := range b.Tools {
		newer.Tools[k] = v
	}
	newer.Tools["c2j"] = strings.Repeat("b", 64)
	if Fingerprint(b) == Fingerprint(newer) {
		t.Fatal("changed tools reused old fingerprint")
	}
	if p.promoteSource(newer, builder, digest, proof) == nil {
		t.Fatal("old proof reinterpreted as new tools")
	}
	if bytes, _ := os.ReadFile(saved.Path); string(bytes) != string(image) {
		t.Fatal("failed preparation damaged previous source")
	}
	// Catalog qualification survives an unavailable/warming copy. Admission still
	// requires successful digest validation of the actual prepared disk.
	p.qualified = func(base Base) bool { _, e := p.sourceProof(base); return e == nil }
	p.ready = p.sourceReady
	os.Remove(saved.Path)
	catalog, e := p.GetCapabilities(bg)
	if e != nil || len(catalog.Bases) != 1 || p.sourceReady(b) {
		t.Fatal("qualified catalog confused with current occupancy")
	}
}
func TestIndependentSupervisorArguments(t *testing.T) {
	p, _ := testRuntime(t)
	p.config.Runner = "/approved/provider"
	n := systemd{p.config}
	s := runner.Spec{Unit: "pulse-owned.service", Identity: "identity", TimeoutSeconds: 2}
	args := strings.Join(n.launchArgs("/private/absolute", s), " ")
	for _, required := range []string{"--property=User=" + strconv.Itoa(os.Geteuid()), "--property=Group=" + strconv.Itoa(os.Getegid()), "--no-block", "--service-type=notify", "NotifyAccess=main", "TimeoutStartSec=1800s", "ExitType=cgroup", "KillMode=control-group", "KillSignal=SIGKILL", "Restart=no", "RuntimeMaxSec=2s", "StandardOutput=null", "-- /approved/provider runner --dir /private/absolute"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing independent supervision property %q", required)
		}
	}
	if strings.Contains(args, "--scope") || strings.Contains(args, syntheticLease) {
		t.Fatal("runner belongs to caller scope or exposes lease")
	}
	s.TimeoutSeconds = 0
	if strings.Contains(strings.Join(n.launchArgs("/private/absolute", s), " "), "RuntimeMaxSec") {
		t.Fatal("uncapped execution given a unit runtime cap")
	}
}
func TestUserManagerKeepsItsOwnIdentity(t *testing.T) {
	p, _ := testRuntime(t)
	p.config.UserManager = true
	args := strings.Join((systemd{p.config}).launchArgs("/private/absolute", runner.Spec{Unit: "owned.service", Identity: "test"}), " ")
	if !strings.HasPrefix(args, "--user ") || strings.Contains(args, "--property=User=") || strings.Contains(args, "--property=Group=") {
		t.Fatal("user-manager launch tried to change service identity")
	}
}

func TestDrainDeclinesWithoutQueueingLease(t *testing.T) {
	p, f := testRuntime(t)
	p.mu.Lock()
	p.draining = true
	p.mu.Unlock()
	if submit(t, p, launch("during-preparation")).Status != "no_capacity" || f.count() != 0 || len(p.records) != 0 {
		t.Fatal("warming maintenance retained a lease")
	}
	p.mu.Lock()
	p.draining = false
	p.mu.Unlock()
	if submit(t, p, launch("after-preparation")).Status != "accepted" {
		t.Fatal("drain did not reopen admission")
	}
}

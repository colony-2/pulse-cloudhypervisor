package cloudhypervisor

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

func TestNestedKVMDiscoveryAdmissionAndBoot(t *testing.T) {
	p, backend := testRuntime(t)
	base := p.config.Bases[0]
	p.config.Hypervisor.SHA256 = strings.Repeat("b", 64)
	boot := "first-boot"
	p.nestedBoot = func() (string, error) { return boot, nil }
	// An operator setting cannot stand in for the guest or host.
	base.Capabilities = []string{"docker", "kvm"}
	p.config.Bases[0] = base
	dir := filepath.Join(p.config.StateDir, "sources", "build-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	disk := []byte("qualified source")
	if err := os.WriteFile(filepath.Join(dir, "disk.raw"), disk, 0600); err != nil {
		t.Fatal(err)
	}
	proof := guest.Proof{SupportsKVMProbe: true, Ubuntu: "24.04", Architecture: base.Architecture, Recipe: base.Recipe, Tools: base.Tools, Docker: true, DockerImage: base.DockerFixtureDigest, CPUMillis: 4000, MemoryBytes: 7 << 30}
	if err := p.promoteSource(base, dir, fmt.Sprintf("%x", sha256.Sum256(disk)), proof); err != nil {
		t.Fatal(err)
	}
	p.ready = p.sourceReady
	p.qualified = func(b Base) bool { _, err := p.sourceProof(b); return err == nil }
	want := launch("nested")
	want.Capabilities = append(want.Capabilities, "kvm")
	check := func(wantKVM bool) {
		t.Helper()
		result, err := p.GetCapabilities(bg)
		if err != nil || len(result.Bases) != 1 || !slices.Contains(result.Bases[0].Capabilities, "docker") || slices.Contains(result.Bases[0].Capabilities, "kvm") != wantKVM {
			t.Fatalf("catalog = %+v, %v", result, err)
		}
		if !wantKVM {
			if got := submit(t, p, want); got.Status != api.Unsupported {
				t.Fatalf("without nested support: %+v", got)
			}
		}
	}
	check(false)
	binding, err := p.kvmBinding(base)
	if err != nil {
		t.Fatal("missing probe input")
	}
	if err := state.Atomic(p.kvmProofPath(base), binding, nil); err != nil {
		t.Fatal(err)
	}
	check(true)
	boot = ""
	check(false)
	boot = "changed-boot"
	check(false)
	boot = binding.Boot
	p.config.KVMGICTraps = true
	check(runtime.GOARCH != "arm64")
	p.config.KVMGICTraps = false
	p.config.Hypervisor.SHA256 = strings.Repeat("c", 64)
	check(false)
	p.config.Hypervisor.SHA256 = binding.Hypervisor
	check(true)
	if backend.count() != 0 {
		t.Fatal("unsupported submissions dispatched")
	}
	if got := submit(t, p, want); got.Status != api.Accepted {
		t.Fatal(got)
	}
	s := backend.starts[0]
	if !s.NestedKVM || !slices.Contains(s.Facts.Capabilities, "kvm") || !slices.Contains(runner.Args("/tmp/test", s, time.Now()), "boot=1,nested=on") {
		t.Fatal("nested dispatch/facts disagree")
	}
	for _, host := range []string{binding.Boot, ""} {
		boot = host
		backend.observation.terminated = true
		if err := p.Reconcile(bg); err != nil {
			t.Fatal(err)
		}
		ordinary := launch(fmt.Sprint("ordinary-", backend.count()))
		if got := submit(t, p, ordinary); got.Status != api.Accepted {
			t.Fatal(got)
		}
		s = backend.starts[len(backend.starts)-1]
		if s.NestedKVM || slices.Contains(s.Facts.Capabilities, "kvm") || !slices.Contains(runner.Args("/tmp/test", s, time.Now()), "boot=1") {
			t.Fatal("ordinary job depends on nested support")
		}
	}
}

func TestKVMTrapsOnlyOnNestedDispatch(t *testing.T) {
	s := runner.Spec{KVMGICTraps: true}
	for _, nested := range []bool{false, true} {
		s.NestedKVM = nested
		args := strings.Join(runner.Args("/tmp/test", s, time.Unix(1, 0)), " ")
		for _, token := range []string{"nested=on", "pulse.nested_kvm=1", "vgic_v3_group0_trap=1", "vgic_v3_group1_trap=1", "vgic_v3_common_trap=1"} {
			if strings.Contains(args, token) != nested {
				t.Fatalf("unexpected %s in %s", token, args)
			}
		}
	}
}

func TestOptionalProbeNeedsThisGuestsProofAndPreservesOrdinarySource(t *testing.T) {
	p, backend := testRuntime(t)
	base := p.config.Bases[0]
	root := filepath.Join(p.config.StateDir, "sources")
	dir := filepath.Join(root, "build-ordinary")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	disk := []byte("unchanged prepared Ubuntu")
	if err := os.WriteFile(filepath.Join(dir, "disk.raw"), disk, 0600); err != nil {
		t.Fatal(err)
	}
	proof := guest.Proof{Ubuntu: "24.04", Architecture: base.Architecture, Recipe: base.Recipe, Tools: base.Tools, Docker: true, DockerImage: base.DockerFixtureDigest, CPUMillis: 4000, MemoryBytes: 7 << 30}
	source := fmt.Sprintf("%x", sha256.Sum256(disk))
	if err := p.promoteSource(base, dir, source, proof); err != nil {
		t.Fatal(err)
	}
	if err := p.removeBuilder(dir); err != nil {
		t.Fatal(err)
	}
	boot, err := runner.BootID()
	if err != nil {
		t.Fatal(err)
	}
	p.nestedBoot = func() (string, error) { return boot, nil }
	if _, err := p.kvmBinding(base); err == nil {
		t.Fatal("legacy guest qualified for a new protocol")
	}
	marker, err := p.sourceProof(base)
	if err != nil {
		t.Fatal(err)
	}
	marker.Proof.SupportsKVMProbe = true
	if err := state.Atomic(filepath.Join(root, Fingerprint(base)+".json"), marker, nil); err != nil {
		t.Fatal(err)
	}
	binding, err := p.kvmBinding(base)
	if err != nil {
		t.Fatal("missing probe binding")
	}
	backend.observation.terminated = true
	p.config.SystemdRun = filepath.Join(t.TempDir(), "systemd-run")
	var report bytes.Buffer
	p.setupOutput = &report
	for _, outcome := range []string{"missing", "wrong", "correct"} {
		report.Reset()
		script := `#!/bin/sh
set -eu
umask 077
while [ "$1" != --dir ]; do shift; done
cd "$2"
test -f plan.json
`
		switch outcome {
		case "wrong":
			script += "echo '{\"KVMIdentity\":\"another-VM\"}' > proof.json\n"
		case "correct":
			script += `python3 - <<'PY'
import json
plan=json.load(open("plan.json"))
record=json.load(open("record.json"))
assert plan["ProbeKVM"] and record["nested_kvm"] and plan["Identity"]==record["identity"] and not plan["Script"]
json.dump({"KVMIdentity":record["identity"]},open("proof.json","w"))
PY
`
		}
		if err := os.WriteFile(p.config.SystemdRun, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		if err := p.buildSource(bg, base, &binding); err != nil {
			t.Fatal(outcome, err)
		}
		if outcome == "correct" && !strings.Contains(report.String(), "Saved proof "+p.kvmProofPath(base)) ||
			outcome != "correct" && !strings.Contains(report.String(), "Guest KVM probe failed") {
			t.Fatalf("%s probe report: %q", outcome, report.String())
		}
		if (p.verifyKVMProof(base, binding) == nil) != (outcome == "correct") || !p.sourceReady(base) || len(p.builders) != 0 {
			t.Fatal(outcome, "incorrect proof, source, or reservation")
		}
	}
	// The qualification command reopens the saved pool with admission drained.
	settings := filepath.Join(p.config.StateDir, "pool.yaml")
	if err := SaveConfig(settings, p.config); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(settings)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	reloaded.Count = &zero
	reopened, err := New(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.nestedBoot = p.nestedBoot
	catalog, err := reopened.GetCapabilities(bg)
	if err != nil || len(catalog.Bases) != 1 || !slices.Contains(catalog.Bases[0].Capabilities, "kvm") {
		t.Fatalf("reopened catalog lost verified guest KVM: %+v, %v", catalog, err)
	}

}

func TestX86NestedSetting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		params   map[string]string
		disabled string
	}{
		{"intel enabled", map[string]string{"kvm_intel": "Y\n"}, ""},
		{"amd enabled", map[string]string{"kvm_amd": "1\n"}, ""},
		{"intel disabled", map[string]string{"kvm_intel": "N\n"}, "kvm_intel/parameters/nested=N"},
		{"amd disabled", map[string]string{"kvm_amd": "0\n"}, "kvm_amd/parameters/nested=0"},
		{"hidden settings still allow a probe", nil, ""},
		{"unknown setting still allows a probe", map[string]string{"kvm_intel": "unknown"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := x86NestedSetting(func(path string) ([]byte, error) {
				module := filepath.Base(filepath.Dir(filepath.Dir(path)))
				if value, ok := tc.params[module]; ok {
					return []byte(value), nil
				}
				return nil, os.ErrPermission
			})
			if tc.disabled == "" && err != nil || tc.disabled != "" && (err == nil || !strings.Contains(err.Error(), tc.disabled)) {
				t.Fatalf("got %v, expected disabled setting %q", err, tc.disabled)
			}
		})
	}
}

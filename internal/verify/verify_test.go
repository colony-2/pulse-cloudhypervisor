package verify

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestVerificationGate(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test location")
	}
	script := filepath.Join(filepath.Dir(file), "../../scripts/verify_test.py")
	for _, name := range []string{"S01", "S02", "S03", "complete_results", "missing_empty_skipped_failed_incomplete", "native_identity_and_pins", "recognized_hook_contract", "hook_script_runs_children_and_propagates_failure", "explicit_acceptance_fails_without_prerequisites", "diagnostics_do_not_export_input", "source_binding", "local_command_and_full_boundary", "local_mapping_partitions", "child_timeout_and_absence", "source_mutations_fail_commands", "S24", "S25"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("python3", script, "VerificationTests.test_"+name)
			if b, e := cmd.CombinedOutput(); e != nil {
				t.Fatalf("verification assertion failed: %v\n%s", e, b)
			}
		})
	}
}

// Package preparation may add a top-level packages collection. It must not
// invalidate the native Ubuntu/KVM execution request or disguise its removal.
func TestRepositoryExecutionSettings(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test location")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../.c2/config.yaml"))
	if err != nil {
		t.Fatal("execution configuration is required")
	}
	check := func(raw []byte) error {
		var settings map[string]any
		if err := yaml.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("invalid repository settings")
		}
		want := map[string]any{"base": "ubuntu*", "capabilities": []any{"kvm"}}
		if !reflect.DeepEqual(settings["execution"], want) {
			return fmt.Errorf("Ubuntu and KVM without universal resources are required")
		}
		if !reflect.DeepEqual(settings["packages"], []any{"nix:github:NixOS/nixpkgs/680275f0bb6afce84fbac3e75fc47c5f2272400b#cloud-hypervisor"}) {
			return fmt.Errorf("approved package is required")
		}
		return nil
	}
	t.Run("current", func(t *testing.T) {
		if err := check(raw); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("approved_package_proposal", func(t *testing.T) {
		// Parse only; this fixture neither installs nor realizes the package.
		if err := check([]byte(`execution:
  base: ubuntu*
  capabilities: [kvm]
packages:
  - nix:github:NixOS/nixpkgs/680275f0bb6afce84fbac3e75fc47c5f2272400b#cloud-hypervisor
`)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reject_invalid_settings", func(t *testing.T) {
		for _, bad := range []string{
			"[", "{}", "execution: {}", string(bytes.Replace(raw, []byte("ubuntu*"), []byte("*"), 1)),
			string(bytes.Replace(raw, []byte("[kvm]"), []byte("[]"), 1)),
			string(bytes.Replace(raw, []byte("  base:"), []byte("  resources: {}\n  base:"), 1)),
			string(bytes.Replace(raw, []byte("packages:"), []byte("unapproved:"), 1)),
		} {
			if check([]byte(bad)) == nil {
				t.Fatal("invalid settings accepted")
			}
		}
	})
}

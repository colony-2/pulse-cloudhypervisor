package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ch "github.com/colony-2/pulse-cloudhypervisor"
)

func TestConfigPrecedenceAndFailedMaintenancePreservesSettings(t *testing.T) {
	root, e := os.MkdirTemp("/tmp", "cli-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	t.Setenv("HOME", "")
	path := filepath.Join(root, "config.json")
	cfg := ch.Config{Version: 1, Owner: "saved", StateDir: filepath.Join(root, "saved-state")}
	if ch.SaveConfig(path, cfg) != nil {
		t.Fatal("save")
	}
	got, e := load(path, "override", filepath.Join(root, "explicit"))
	if e != nil || got.Owner != "override" || got.StateDir != filepath.Join(root, "explicit") {
		t.Fatal("flag precedence")
	}
	before, _ := os.ReadFile(path)
	for _, mode := range []string{"setup", "upgrade"} {
		if run(context.Background(), []string{mode, "--config", path}) == nil {
			t.Fatal("unqualified setup succeeded")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("failed maintenance changed settings")
		}
	}
	if run(context.Background(), []string{"setup", "--config", path, "--unknown"}) == nil || run(context.Background(), []string{"unknown"}) == nil {
		t.Fatal("unknown CLI accepted")
	}
	os.WriteFile(path, []byte(`{"version":1,"owner":"saved","state_dir":"relative"}`), 0600)
	got, e = load(path, "", "")
	if e != nil || got.StateDir != filepath.Join(root, "relative") {
		t.Fatal("saved relative path")
	}
	os.WriteFile(path, []byte(`{"version":2,"owner":"saved","state_dir":"relative"}`), 0600)
	if _, e = load(path, "", ""); e == nil {
		t.Fatal("unknown version accepted")
	}
	os.WriteFile(path, []byte(`{"version":1,"owner":"saved","private-secret":"test"}`), 0600)
	if _, e = load(path, "", ""); e == nil || strings.Contains(e.Error(), "test") {
		t.Fatal("unknown config or secret error")
	}
}

func TestSuccessfulDrainedSetupSavesResolvedSettings(t *testing.T) {
	root, e := os.MkdirTemp("/tmp", "saved-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	t.Setenv("HOME", root)
	path := filepath.Join(root, "settings.json")
	zero := 0
	cfg := ch.Config{Version: 1, Owner: "drained", Count: &zero}
	if ch.SaveConfig(path, cfg) != nil {
		t.Fatal("initial settings")
	}
	if e = run(context.Background(), []string{"setup", "--config", path}); e != nil {
		t.Fatal(e)
	}
	saved, e := ch.LoadConfig(path)
	if e != nil || !filepath.IsAbs(saved.StateDir) || saved.StateDir != filepath.Join(root, ".pulse/cloudhypervisor/owners/drained") {
		t.Fatal("resolved settings not saved atomically")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("saved settings not private")
	}
}

func TestCapacityFlagsPersistOverridesAndExplicitZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	cfg := ch.Config{Version: 1, Owner: "test", StateDir: filepath.Join("/tmp", "unused"), HostCPUMillis: 9000}
	if err := ch.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	// The short owner path is private to this invocation.
	stateDir, err := os.MkdirTemp("/tmp", "ch-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	err = run(context.Background(), []string{"setup", "--config", path, "--state-dir", stateDir, "-l", "5", "-c", "0", "--total-memory-bytes", "4294967296", "--max-worker-cpu-millis", "2500", "--max-worker-memory-bytes", "2147483648", "--default-worker-cpu-millis", "1000", "--default-worker-memory-bytes", "1073741824"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ch.LoadConfig(path)
	if err != nil || *got.Count != 5 || *got.TotalCapacity != 0 || got.HostMemoryBytes != 4<<30 || got.HostCPUMillis != 9000 || got.MaxWorkerCPUMillis != 2500 || got.MaxWorkerMemoryBytes != 2<<30 || got.DefaultWorkerCPUMillis != 1000 || got.DefaultWorkerMemoryBytes != 1<<30 {
		t.Fatal(got, err)
	}
	if err = run(context.Background(), []string{"upgrade", "--config", path, "--default-worker-cpu-millis", "2501"}); err == nil {
		t.Fatal("oversized default saved")
	}
}

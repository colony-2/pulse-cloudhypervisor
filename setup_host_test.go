package cloudhypervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstSetupAndYAMLOverrides(t *testing.T) {
	home := shortDir(t)
	t.Setenv("HOME", home)
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	var report bytes.Buffer
	if err := MaintainHost(context.Background(), path, true, false, func(c *Config) { c.Count = &zero }, &report); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.Owner != "default" || c.Provision == nil || !c.Provision.Base || !c.Provision.Network ||
		c.HostCPUMillis <= 0 || c.HostMemoryBytes <= 0 || c.HostDiskBytes <= 0 || *c.Count != 0 {
		t.Fatalf("incomplete generated pool: %+v, err=%v", c, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("owner: default\n")) || !strings.Contains(report.String(), "Saved settings to "+path) {
		t.Fatal("missing YAML or report", err, report.String())
	}
	if err := MaintainHost(context.Background(), path, true, false, nil, nil); err != nil {
		t.Fatal("rerun of generated YAML", err)
	}
	// An ordinary checked-in config can override individual fields.
	dir := filepath.Join(home, "checkout")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(dir, "pool.yaml")
	if err := os.WriteFile(override, []byte("owner: custom\nstate_dir: data\ncount: 0\nhost_cpu_millis: 2000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err = MaintainHost(context.Background(), override, false, false, func(c *Config) { c.HostCPUMillis = 1000 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(override)
	if err != nil || c.StateDir != filepath.Join(dir, "data") || c.Owner != "custom" || c.HostCPUMillis != 1000 {
		t.Fatal("operator YAML and command-line override", c, err)
	}
	if st, err := os.Stat(override); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("settings publication mode", err)
	}
	before, _ := os.ReadFile(override)
	err = MaintainHost(context.Background(), override, false, false, func(c *Config) { c.Version = 99 }, nil)
	after, _ := os.ReadFile(override)
	if err == nil || !bytes.Equal(before, after) {
		t.Fatal("failed setup replaced operator settings")
	}
}

func TestYAMLStrictnessAndLegacyDefault(t *testing.T) {
	for _, raw := range []string{"null", "owner: x\n---\n{}", "owner: x\nowner: y", "unknown_field: true", "count: many"} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Errorf("accepted invalid YAML %q", raw)
		}
	}
	home := shortDir(t)
	t.Setenv("HOME", home)
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(filepath.Dir(path), "config.json")
	zero := 0
	if err := SaveConfig(legacy, Config{Version: 1, Owner: "existing", Count: &zero}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(legacy)
	if err := MaintainHost(context.Background(), path, true, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	after, _ := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(before, after) || c.Owner != "existing" {
		t.Fatal("legacy pool migration", err)
	}
	selected, err := ConfigInput(path, true)
	if selected != path || err != nil {
		t.Fatal("YAML should take precedence", selected, err)
	}
	typo := filepath.Join(home, "typo.yaml")
	if err := MaintainHost(context.Background(), typo, false, false, nil, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing explicit config must fail", err)
	}
}

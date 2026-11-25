package cloudhypervisor

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	"gopkg.in/yaml.v3"
)

// The installed CLI can prepare a guest from the same sources as the host
// binary, including when installed with go install or launched with go run.
//
//go:embed go.mod go.sum cmd/pulse-cloudhypervisor-guest/main.go internal/guest/agent.go internal/guest/prepare.go internal/guest/network.go internal/guest/kvm.go internal/guest/kvm_setup.py internal/protocol/protocol.go internal/state/store.go guest/pulse-cloudhypervisor-guest.service scripts/native_assets.py scripts/native_setup.py scripts/setup/*.py scripts/setup/*.sh
var hostSetupFiles embed.FS

// MaintainHost serializes first-run discovery, host provisioning, VM preparation
// and settings publication. A failed operation leaves the previous YAML intact.
func MaintainHost(ctx context.Context, settings string, legacy, upgrade bool, edit func(*Config), report io.Writer) error {
	dir := filepath.Dir(settings)
	if err := state.PrivateDir(dir); err != nil {
		return err
	}
	lock, err := state.Lock(filepath.Join(dir, ".setup-"+key(settings)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	input, err := ConfigInput(settings, legacy)
	if os.IsNotExist(err) && !upgrade {
		input = ""
	} else if err != nil {
		return fmt.Errorf("read settings %s: %w", settings, err)
	}
	c, present, err := setupInput(input, edit)
	if err != nil {
		return err
	}
	// Hold the real pool's owner lock before changing its host resources.
	owner, err := New(Config{Version: c.Version, Owner: c.Owner, StateDir: c.StateDir, Systemctl: c.Systemctl, UserManager: c.UserManager})
	if err != nil {
		return err
	}
	defer owner.Close()
	if len(owner.records)+len(owner.builders) > 0 && !upgrade {
		return ErrPreparation
	}
	if input == "" {
		fmt.Fprintf(report, "Choosing native defaults for %s\n", settings)
	} else {
		fmt.Fprintf(report, "Using settings from %s\n", input)
	}
	c, err = hostSettings(ctx, c, present, settings, edit, report)
	if err != nil {
		return err
	}
	owner.config, owner.backend = c, systemd{c}
	fmt.Fprintf(report, "Preparing owner %s with %d slot(s), %d vCPUs and %d MiB per reference VM\n", c.Owner, c.workerCount(), c.VCPUs, c.MemoryBytes>>20)
	if upgrade {
		err = owner.Upgrade(ctx)
	} else {
		err = owner.Setup(ctx)
	}
	if err != nil {
		return err
	}
	if err = SaveConfig(settings, c); err != nil {
		return err
	}
	fmt.Fprintf(report, "Saved settings to %s\n", settings)
	return nil
}

func setupInput(input string, edit func(*Config)) (Config, map[string]yaml.Node, error) {
	var c Config
	present := map[string]yaml.Node{}
	if input != "" {
		f, err := state.OpenPrivate(input, os.O_RDONLY)
		if err != nil {
			return c, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		f.Close()
		if err != nil {
			return c, nil, err
		}
		c, err = DecodeConfig(data)
		if err != nil || yaml.Unmarshal(data, &present) != nil {
			return c, nil, ErrConfig
		}
		c = resolvePaths(c, filepath.Dir(input))
	}
	if _, ok := present["version"]; !ok {
		c.Version = ConfigVersion
	}
	if _, ok := present["owner"]; !ok {
		c.Owner = "default"
	}
	edit(&c)
	if c.Version != ConfigVersion || !nameRE.MatchString(c.Owner) || c.Owner == "." || c.Owner == ".." {
		return c, nil, ErrConfig
	}
	if c.StateDir == "" {
		h, err := home()
		if err != nil {
			return c, nil, err
		}
		c.StateDir = filepath.Join(h, ".pulse/cloudhypervisor/owners", c.Owner)
		if len(c.StateDir) > 60 {
			c.StateDir = filepath.Join("/var/tmp", fmt.Sprintf("pulse-ch-%d-%s", os.Getuid(), key(c.StateDir)[:12]))
		}
	}
	return c, present, nil
}

func hostSettings(ctx context.Context, c Config, present map[string]yaml.Node, settings string, edit func(*Config), report io.Writer) (Config, error) {
	if (c.Count != nil && *c.Count == 0 || c.TotalCapacity != nil && *c.TotalCapacity == 0) && present["host_cpu_millis"].Kind != 0 && present["host_memory_bytes"].Kind != 0 && present["host_disk_bytes"].Kind != 0 {
		return Normalize(c)
	}
	// A fully operator-specified pool keeps its topology and pinned inputs.
	if c.Provision == nil && len(c.Bases) > 0 && len(c.Network) > 0 && c.Runner != "" && c.Hypervisor.Path != "" && c.SystemdRun != "" && c.Systemctl != "" && present["host_cpu_millis"].Kind != 0 && present["host_memory_bytes"].Kind != 0 && present["host_disk_bytes"].Kind != 0 {
		return Normalize(c)
	}
	if !filepath.IsAbs(c.StateDir) || len(c.StateDir) > 60 || filepath.Clean(c.StateDir) != c.StateDir {
		return c, ErrConfig
	}
	if err := state.PrivateDir(c.StateDir); err != nil {
		return c, err
	}
	dir, err := os.MkdirTemp("", "pulse-ch-setup-")
	if err != nil {
		return c, err
	}
	defer os.RemoveAll(dir)
	err = fs.WalkDir(hostSetupFiles, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		data, err := hostSetupFiles.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	})
	if err != nil {
		return c, err
	}
	exe, err := os.Executable()
	if err != nil {
		return c, err
	}
	fields := make([]string, 0, len(present))
	for k := range present {
		fields = append(fields, k)
	}
	payload, err := json.Marshal(struct {
		Config               Config
		Present              []string
		Settings, Executable string
	}{c, fields, settings, exe})
	if err != nil {
		return c, err
	}
	request := filepath.Join(dir, "request")
	result := filepath.Join(dir, "result")
	if err = os.WriteFile(request, payload, 0600); err != nil {
		return c, err
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		return c, errors.New("first-run setup requires Python 3")
	}
	cmd := exec.CommandContext(ctx, py, "-I", filepath.Join(dir, "scripts/setup/host.py"), request, result)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = report, report, os.Stdin
	if err = cmd.Run(); err != nil {
		return c, fmt.Errorf("host provisioning: %w", err)
	}
	c, err = LoadConfig(result)
	if err != nil {
		return c, err
	}
	edit(&c)
	return Normalize(c)
}

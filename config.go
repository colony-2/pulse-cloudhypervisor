// Package cloudhypervisor implements disposable Linux KVM workers. Accepted
// execution belongs to independent systemd runners, not the Provider lifetime.
package cloudhypervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

const ConfigVersion = 1

var ErrConfig = errors.New("cloudhypervisor: invalid configuration")
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Asset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Base struct {
	Name         string `json:"name"`
	Architecture string `json:"architecture"`
	Disk         Asset  `json:"disk"`
	Kernel       Asset  `json:"kernel"`
	Initramfs    Asset  `json:"initramfs"`
	// Recipe and Tools are immutable content/revision pins; no latest/main.
	Recipe              string            `json:"recipe"`
	Preparation         Asset             `json:"preparation"`
	DockerFixtureDigest string            `json:"docker_fixture_digest"`
	Tools               map[string]string `json:"tools"`
	Capabilities        []string          `json:"capabilities"`
	UsableCPUMillis     int64             `json:"usable_cpu_millis"`
	UsableMemoryBytes   int64             `json:"usable_memory_bytes"`
}
type Network struct {
	TAP     string `json:"tap"`
	Address string `json:"address"`
	Gateway string `json:"gateway"`
	Netmask string `json:"netmask"`
	DNS     string `json:"dns"`
}
type Config struct {
	Version                  int       `json:"version"`
	Owner                    string    `json:"owner"`
	StateDir                 string    `json:"state_dir"`
	Count                    *int      `json:"count,omitempty"`
	TotalCapacity            *int      `json:"total_capacity,omitempty"`
	MaxWorkerCPUMillis       int64     `json:"max_worker_cpu_millis,omitempty"`
	MaxWorkerMemoryBytes     int64     `json:"max_worker_memory_bytes,omitempty"`
	DefaultWorkerCPUMillis   int64     `json:"default_worker_cpu_millis,omitempty"`
	DefaultWorkerMemoryBytes int64     `json:"default_worker_memory_bytes,omitempty"`
	VCPUs                    int       `json:"vcpus"`
	MemoryBytes              int64     `json:"memory_bytes"`
	DiskBytes                int64     `json:"disk_bytes"`
	HostCPUMillis            int64     `json:"host_cpu_millis"`
	HostMemoryBytes          int64     `json:"host_memory_bytes"`
	HostDiskBytes            int64     `json:"host_disk_bytes"`
	Hypervisor               Asset     `json:"hypervisor"`
	Runner                   string    `json:"runner"`
	SystemdRun               string    `json:"systemd_run"`
	Systemctl                string    `json:"systemctl"`
	KVMGICTraps              bool      `json:"kvm_gic_traps,omitempty"`
	UserManager              bool      `json:"user_manager"`
	Bases                    []Base    `json:"bases"`
	Network                  []Network `json:"network"`
}

// PathResolver shares one lazy home lookup across command settings and owner
// defaults. Its zero value is ready for use and safe for concurrent callers.
type PathResolver struct {
	once   sync.Once
	value  string
	err    error
	lookup func() (string, error)
}

func (r *PathResolver) defaultHome() (string, error) {
	r.once.Do(func() {
		if r.lookup != nil {
			r.value, r.err = r.lookup()
		} else {
			r.value, r.err = home()
		}
		if !filepath.IsAbs(r.value) || filepath.Clean(r.value) != r.value {
			r.err = ErrConfig
		}
	})
	return r.value, r.err
}
func (r *PathResolver) ConfigPath(path string) (string, error) {
	if path != "" {
		if !filepath.IsAbs(path) {
			return "", ErrConfig
		}
		return path, nil
	}
	h, e := r.defaultHome()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, ".pulse/cloudhypervisor/config.json"), nil
}
func (r *PathResolver) Normalize(c Config) (Config, error) {
	if c.Version != ConfigVersion || !nameRE.MatchString(c.Owner) {
		return c, ErrConfig
	}
	if c.StateDir == "" {
		h, e := r.defaultHome()
		if e != nil {
			return c, e
		}
		c.StateDir = filepath.Join(h, ".pulse/cloudhypervisor/owners", c.Owner)
	}
	return normalize(c)
}
func DefaultConfigPath() (string, error) { var r PathResolver; return r.ConfigPath("") }
func Normalize(c Config) (Config, error) { var r PathResolver; return r.Normalize(c) }

func home() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil || !filepath.IsAbs(h) || filepath.Clean(h) != h {
		return "", ErrConfig
	}
	info, err := os.Lstat(h)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrConfig
	}
	return h, nil
}
func normalize(c Config) (Config, error) {
	if c.Version != ConfigVersion || !nameRE.MatchString(c.Owner) || c.Owner == "." || c.Owner == ".." {
		return c, ErrConfig
	}

	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || len(c.StateDir) > 60 {
		return c, ErrConfig
	}
	// Use a short explicit directory when a home would exceed Unix socket limits.
	if c.Count == nil {
		v := 1
		c.Count = &v
	} else {
		v := *c.Count
		c.Count = &v
	}
	if *c.Count < 0 || *c.Count > 1024 {
		return c, ErrConfig
	}
	if c.TotalCapacity != nil {
		total := *c.TotalCapacity
		if total < 0 || total > 1024 {
			return c, ErrConfig
		}
		c.TotalCapacity = &total
	}
	if c.VCPUs == 0 {
		c.VCPUs = 4
	}
	if c.MemoryBytes == 0 {
		c.MemoryBytes = 8 << 30
	}
	if c.DiskBytes == 0 {
		c.DiskBytes = 32 << 30
	}
	if c.VCPUs < 1 || c.VCPUs > 1024 || c.MemoryBytes < 1<<30 || c.MemoryBytes > 1<<50 || c.DiskBytes < 1<<30 || c.DiskBytes > 1<<50 {
		return c, ErrConfig
	}
	for _, n := range []int64{c.HostCPUMillis, c.HostMemoryBytes, c.HostDiskBytes, c.MaxWorkerCPUMillis, c.MaxWorkerMemoryBytes, c.DefaultWorkerCPUMillis, c.DefaultWorkerMemoryBytes} {
		if n < 0 || n > 9007199254740991 {
			return c, ErrConfig
		}
	}
	if c.DefaultWorkerCPUMillis > limit(int64(c.VCPUs)*1000, c.MaxWorkerCPUMillis) || c.DefaultWorkerMemoryBytes > limit(c.MemoryBytes, c.MaxWorkerMemoryBytes) {
		return c, ErrConfig
	}
	for _, p := range []string{c.Runner, c.SystemdRun, c.Systemctl, c.Hypervisor.Path} {
		if p != "" && (!filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n\r")) {
			return c, ErrConfig
		}
	}
	c.Bases = append([]Base(nil), c.Bases...)
	names := map[string]bool{}
	for i, b := range c.Bases {
		if (b.Architecture != "arm64" && b.Architecture != "amd64") || b.Name != "ubuntu-24.04-"+b.Architecture || names[b.Name] || b.UsableCPUMillis < 1 || b.UsableCPUMillis > int64(c.VCPUs)*1000 || b.UsableMemoryBytes < 1 || b.UsableMemoryBytes >= c.MemoryBytes {
			return c, ErrConfig
		}
		if nativeBase(b) && (c.DefaultWorkerCPUMillis > b.UsableCPUMillis || c.DefaultWorkerMemoryBytes > b.UsableMemoryBytes) {
			return c, ErrConfig
		}
		names[b.Name] = true
		if !strings.HasPrefix(b.DockerFixtureDigest, "sha256:") || len(b.DockerFixtureDigest) != 71 || !pin(strings.TrimPrefix(b.DockerFixtureDigest, "sha256:")) {
			return c, ErrConfig
		}
		if !pin(b.Recipe) || !validAsset(b.Preparation) || b.Preparation.SHA256 != b.Recipe {
			return c, ErrConfig
		}
		tools := map[string]string{}
		for k, v := range b.Tools {
			if !nameRE.MatchString(k) || len(v) != 64 || !pin(v) {
				return c, ErrConfig
			}
			tools[k] = v
		}
		for _, k := range []string{"c2j", "nix", "docker", "guest-agent"} {
			if tools[k] == "" {
				return c, ErrConfig
			}
		}
		b.Tools = tools
		b.Capabilities = append([]string(nil), b.Capabilities...)
		for _, cap := range b.Capabilities {
			if cap != "docker" && cap != "kvm" {
				return c, ErrConfig
			}
		}
		for _, a := range []Asset{b.Disk, b.Kernel} {
			if !validAsset(a) {
				return c, ErrConfig
			}
		}
		if b.Initramfs.Path != "" && !validAsset(b.Initramfs) {
			return c, ErrConfig
		}
		c.Bases[i] = b
	}
	c.Network = append([]Network(nil), c.Network...)
	seen := map[string]bool{}
	for _, n := range c.Network {
		if net.ParseIP(n.Address).To4() == nil || net.ParseIP(n.Gateway).To4() == nil || net.ParseIP(n.Netmask).To4() == nil || net.ParseIP(n.DNS).To4() == nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`).MatchString(n.TAP) || seen[n.TAP] || seen[n.Address] {
			return c, ErrConfig
		}
		if _, bits := net.IPMask(net.ParseIP(n.Netmask).To4()).Size(); bits != 32 {
			return c, ErrConfig
		}
		seen[n.TAP] = true
		seen[n.Address] = true
	}
	return c, nil
}
func pin(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && (len(b) == 20 || len(b) == 32)
}
func validAsset(a Asset) bool {
	return filepath.IsAbs(a.Path) && filepath.Clean(a.Path) == a.Path && len(a.SHA256) == 64 && pin(a.SHA256)
}
func Fingerprint(b Base) string {
	data, _ := json.Marshal(b)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func nativeBase(b Base) bool { return b.Architecture == runtime.GOARCH }

// LoadConfig applies saved relative paths against the settings directory. Errors
// contain no configuration values. The library constructor never reads this file.
func LoadConfig(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) {
		return c, ErrConfig
	}
	f, e := state.OpenPrivate(path, os.O_RDONLY)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return c, ErrConfig
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, ErrConfig
	}
	c = resolvePaths(c, filepath.Dir(path))
	return c, nil
}
func SaveConfig(path string, c Config) error {
	if !filepath.IsAbs(path) {
		return ErrConfig
	}
	if e := state.PrivateDir(filepath.Dir(path)); e != nil {
		return e
	}
	return state.Atomic(path, c, nil)
}
func DecodeConfig(data []byte) (Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return c, ErrConfig
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return c, ErrConfig
	}
	return c, nil
}

func resolvePaths(c Config, root string) Config {
	resolve := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(root, *p)
		}
	}
	resolve(&c.StateDir)
	resolve(&c.Runner)
	resolve(&c.SystemdRun)
	resolve(&c.Systemctl)
	resolve(&c.Hypervisor.Path)
	for i := range c.Bases {
		resolve(&c.Bases[i].Disk.Path)
		resolve(&c.Bases[i].Kernel.Path)
		resolve(&c.Bases[i].Initramfs.Path)
		resolve(&c.Bases[i].Preparation.Path)
	}
	return c
}

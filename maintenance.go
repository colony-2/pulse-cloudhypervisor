package cloudhypervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

var ErrPreparation = errors.New("cloudhypervisor: preparation failed or prerequisites missing")

type prepared struct {
	Version                   int
	Fingerprint, Path, Digest string
	Proof                     guest.Proof
}

func (p prepared) matches(b Base) bool {
	if p.Version != 1 || p.Fingerprint != Fingerprint(b) || p.Proof.Ubuntu != "24.04" || p.Proof.Architecture != b.Architecture || p.Proof.Recipe != b.Recipe || !p.Proof.Docker || p.Proof.DockerImage != b.DockerFixtureDigest || p.Proof.CPUMillis < b.UsableCPUMillis || p.Proof.MemoryBytes < b.UsableMemoryBytes {
		return false
	}
	for k, v := range b.Tools {
		if p.Proof.Tools[k] != v {
			return false
		}
	}
	return true
}

// Setup prepares missing immutable sources. It holds admission while reserving
// a builder and performs no host package, network or service installation.
func (p *Runtime) maintenanceContext(ctx context.Context) (context.Context, func()) {
	combined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.maintenanceCtx, cancel)
	return combined, func() { stop(); cancel() }
}
func (p *Runtime) Setup(ctx context.Context) error {
	if ctx == nil {
		return ErrBatch
	}
	if !p.maintenance.TryLock() {
		return ErrPreparation
	}
	defer p.maintenance.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if len(p.records)+len(p.builders) > 0 {
		p.mu.Unlock()
		return ErrPreparation
	}
	p.draining = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.draining = false; p.mu.Unlock() }()
	ctx, cancel := p.maintenanceContext(ctx)
	defer cancel()
	return p.prepare(ctx)
}
func (p *Runtime) Upgrade(ctx context.Context) error {
	if ctx == nil {
		return ErrBatch
	}
	if !p.maintenance.TryLock() {
		return ErrPreparation
	}
	defer p.maintenance.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.draining = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.draining = false; p.mu.Unlock() }()
	ctx, cancel := p.maintenanceContext(ctx)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := p.Reconcile(ctx); e != nil {
			return e
		}
		p.mu.Lock()
		empty := len(p.records)+len(p.builders) == 0
		p.mu.Unlock()
		if empty {
			return p.prepare(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (p *Runtime) prepare(ctx context.Context) error {
	p.mu.Lock()
	if p.closed || len(p.builders) > 0 {
		p.mu.Unlock()
		return ErrPreparation
	}
	c := p.config
	p.building = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.building = false; p.mu.Unlock() }()
	if *c.Count == 0 {
		return nil
	}
	if len(c.Network) == 0 || c.HostCPUMillis < int64(c.VCPUs)*1000 || c.HostMemoryBytes < c.MemoryBytes || c.HostDiskBytes < c.DiskBytes || !validAsset(c.Hypervisor) || c.Runner == "" || c.SystemdRun == "" || c.Systemctl == "" {
		return ErrPreparation
	}
	if (systemd{c}).preflight(ctx, 0) != nil {
		return ErrPreparation
	}
	kvm, e := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if e != nil {
		return ErrPreparation
	}
	kvm.Close()
	if images.Verify(c.Hypervisor.Path, c.Hypervisor.SHA256) != nil {
		return ErrPreparation
	}
	root := filepath.Join(c.StateDir, "sources")
	if state.PrivateDir(root) != nil {
		return ErrPreparation
	}
	for _, b := range c.Bases {
		if !nativeBase(b) || p.ready(b) {
			continue
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		for _, a := range []Asset{b.Disk, b.Kernel, b.Preparation} {
			if images.Verify(a.Path, a.SHA256) != nil {
				return ErrPreparation
			}
		}
		dir := filepath.Join(root, "build-"+identity()[:8])
		if state.PrivateDir(dir) != nil {
			return ErrPreparation
		}
		boot, e := runner.BootID()
		if e != nil {
			return e
		}
		id := identity()
		n := c.Network[0]
		s := runner.Spec{Version: 1, Owner: c.Owner, LaunchID: "prepare", Identity: id, BootID: boot, Unit: "pulse-ch-build-" + key(dir) + ".service", Hypervisor: c.Hypervisor.Path, HypervisorDigest: c.Hypervisor.SHA256, Source: b.Disk.Path, SourceDigest: b.Disk.SHA256, Kernel: b.Kernel.Path, KernelDigest: b.Kernel.SHA256, Initramfs: b.Initramfs.Path, InitramfsDigest: b.Initramfs.SHA256, VCPUs: c.VCPUs, MemoryBytes: c.MemoryBytes, DiskBytes: c.DiskBytes, TimeoutSeconds: 1800, TAP: n.TAP, Address: n.Address, Gateway: n.Gateway, Netmask: n.Netmask, DNS: n.DNS, MAC: "02:00:00:00:00:01", CID: 3}
		script, e := os.ReadFile(b.Preparation.Path)
		if e != nil {
			return ErrPreparation
		}
		plan := guest.Preparation{Identity: id, Recipe: b.Recipe, Script: string(script), Tools: b.Tools, DockerImage: b.DockerFixtureDigest}
		p.mu.Lock()
		if p.closed || ctx.Err() != nil {
			p.mu.Unlock()
			return ErrPreparation
		}
		p.builders[dir] = s
		p.mu.Unlock()
		lockFile, lockErr := state.OpenPrivate(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_RDWR)
		if lockErr != nil {
			return ErrPreparation
		}
		lockFile.Close()
		if state.Atomic(filepath.Join(dir, "record.json"), s, nil) != nil || state.Atomic(filepath.Join(dir, "plan.json"), plan, nil) != nil {
			return ErrPreparation
		}
		// The builder command has no assignment or lease. systemd owns its cap and
		// descendants even when setup is interrupted. Never delete an uncertain build.
		args := (systemd{c}).serviceArgs()
		args = append(args, "--no-block", "--quiet", "--service-type=exec", "--unit="+s.Unit, "--property=Description=pulse:"+id, "--property=KillMode=control-group", "--property=RuntimeMaxSec=1800", "--property=StandardOutput=null", "--property=StandardError=null", "--property=RemainAfterExit=yes", "--", c.Runner, "prepare-runner", "--dir", dir, "--digest", runner.Digest(s))
		if exec.CommandContext(ctx, c.SystemdRun, args...).Run() != nil {
			return ErrPreparation
		}
		waitCtx, cancelWait := context.WithTimeout(ctx, 31*time.Minute)
		tick := time.NewTicker(100 * time.Millisecond)
		terminated := false
		for !terminated {
			observed, err := (systemd{c}).observe(waitCtx, dir, s)
			if err == nil && observed.terminated {
				terminated = true
				break
			}
			select {
			case <-waitCtx.Done():
				tick.Stop()
				cancelWait()
				return ErrPreparation
			case <-tick.C:
			}
		}
		tick.Stop()
		cancelWait()

		var proof guest.Proof
		if state.Read(filepath.Join(dir, "proof.json"), &proof) != nil {
			return ErrPreparation
		}
		disk := filepath.Join(dir, "disk.raw")
		f, e := os.Open(disk)
		if e != nil {
			return ErrPreparation
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return ErrPreparation
		}
		digest := hex.EncodeToString(h.Sum(nil))
		p.mu.Lock()
		if p.closed || ctx.Err() != nil {
			p.mu.Unlock()
			return ErrPreparation
		}
		e = p.promoteSource(b, dir, digest, proof)
		if e == nil {
			e = p.removeBuilder(dir)
		}
		p.mu.Unlock()
		if e != nil {
			return e
		}

	}
	return nil
}

// PreparedProof is deliberately not configurable; only a successful isolated
// preparation run writes a source marker. Raw image config never proves tools.
func (p *Runtime) preparedSource(b Base) (string, string) {
	var proof prepared
	if v, e := p.sourceProof(b); e == nil {
		proof = v
		return proof.Path, proof.Digest
	}
	return b.Disk.Path, b.Disk.SHA256
}

func (p *Runtime) loadBuilders() error {
	root := filepath.Join(p.config.StateDir, "sources")
	entries, e := os.ReadDir(root)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return state.ErrCorrupt
	}
	if state.PrivateDir(root) != nil {
		return state.ErrUnsafe
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "build-") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || state.PrivateDir(dir) != nil {
			return state.ErrCorrupt
		}
		s, e := runner.ReadSpec(dir)
		if e != nil || s.Owner != p.config.Owner || s.LaunchID != "prepare" {
			return state.ErrCorrupt
		}
		p.builders[dir] = s
	}
	return nil
}
func (p *Runtime) reconcileBuilders(ctx context.Context) error {
	if p.building {
		return nil
	}
	for dir, s := range p.builders {
		o, e := p.backend.observe(ctx, dir, s)
		if e != nil {
			return e
		}
		if !o.terminated {
			continue
		}
		if e = p.removeBuilder(dir); e != nil {
			return e
		}
	}
	return nil
}
func (p *Runtime) removeBuilder(dir string) error {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return state.ErrUnsafe
	}
	allowed := map[string]bool{"record.json": true, "plan.json": true, "proof.json": true, "runner.lock": true, "start.fence": true, "disk.raw": true, "api": true, "vsock": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return state.ErrUnsafe
		}
	}
	for _, entry := range entries {
		if os.Remove(filepath.Join(dir, entry.Name())) != nil {
			return state.ErrStorage
		}
	}
	if os.Remove(dir) != nil {
		return state.ErrStorage
	}
	delete(p.builders, dir)
	return nil
}

// promoteSource runs only after exact native termination evidence. Failed
// validation or metadata commits preserve the previous immutable source.
func (p *Runtime) promoteSource(b Base, dir, digest string, proof guest.Proof) error {
	root := filepath.Join(p.config.StateDir, "sources")
	if len(digest) != 64 || !pin(digest) {
		return ErrPreparation
	}
	dst := filepath.Join(root, Fingerprint(b)+"-"+digest[:12]+".raw")
	result := prepared{1, Fingerprint(b), dst, digest, proof}
	if !result.matches(b) {
		return ErrPreparation
	}
	disk := filepath.Join(dir, "disk.raw")
	if images.Verify(disk, digest) != nil {
		return ErrPreparation
	}
	if _, e := os.Lstat(dst); e == nil {
		if images.Verify(dst, digest) != nil {
			return ErrPreparation
		}
	} else if !os.IsNotExist(e) {
		return ErrPreparation
	} else if os.Rename(disk, dst) != nil {
		return ErrPreparation
	}
	if os.Chmod(dst, 0400) != nil {
		return ErrPreparation
	}
	f, e := os.Open(dst)
	if e != nil {
		return ErrPreparation
	}
	e = f.Sync()
	f.Close()
	if e != nil {
		return ErrPreparation
	}
	if state.Atomic(filepath.Join(root, Fingerprint(b)+".json"), result, p.writeHook) != nil {
		return ErrPreparation
	}
	return nil
}

package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

func Prepare(dir, digest string) error {
	if state.PrivateDir(dir) != nil {
		return ErrRunner
	}
	s, e := ReadSpec(dir)
	if e != nil || Digest(s) != digest {
		return ErrRunner
	}
	boot, e := BootID()
	if e != nil || boot != s.BootID {
		return ErrRunner
	}
	lock, e := state.Lock(filepath.Join(dir, "runner.lock"))
	if e != nil {
		return ErrRunner
	}
	defer lock.Close()
	if state.Fence(filepath.Join(dir, "start.fence")) != nil {
		return ErrRunner
	}
	// Diagnostic milestones contain no plan, child output or assignment data.
	// Only the separate proof and a stopped builder can qualify a source.
	mark := func(phase string) {
		_ = state.Atomic(filepath.Join(dir, "status.json"), Status{Version: 1, Identity: s.Identity, Phase: phase}, nil)
	}
	mark("checking-inputs")
	var plan guest.Preparation
	if state.Read(filepath.Join(dir, "plan.json"), &plan) != nil || plan.Identity != s.Identity || plan.ProbeKVM != s.NestedKVM {
		return ErrRunner
	}
	for _, a := range [][2]string{{s.Source, s.SourceDigest}, {s.Kernel, s.KernelDigest}, {s.Hypervisor, s.HypervisorDigest}} {
		if images.Verify(a[0], a[1]) != nil {
			return ErrRunner
		}
	}
	if s.Initramfs != "" && images.Verify(s.Initramfs, s.InitramfsDigest) != nil {
		return ErrRunner
	}
	if images.Clone(s.Source, filepath.Join(dir, "disk.raw"), s.DiskBytes) != nil {
		return ErrRunner
	}
	mark("disk-cloned")
	args := Args(dir, s, time.Now())
	for i, v := range args {
		if v == "--cmdline" {
			mode := "pulse.prepare="
			if s.NestedKVM {
				mode = "pulse.kvm_probe="
			}
			args[i+1] = strings.ReplaceAll(args[i+1], "pulse.launch=prepare", mode+s.Identity)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Hypervisor, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if cmd.Start() != nil {
		return ErrRunner
	}
	mark("vmm-started")
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	// This first boot also expands the filesystem and installs the guest
	// service. There is no assignment in the builder or its handoff.
	c, e := waitGuest(filepath.Join(dir, "vsock"), done, 5*time.Minute)
	if e != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	defer c.Close()
	mark("vsock-connected")
	_ = c.SetDeadline(time.Now().Add(29 * time.Minute))
	data, _ := json.Marshal(plan)
	if protocol.Write(c, protocol.Message{Kind: "prepare", Data: data}) != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	mark("plan-delivered")
	m, e := protocol.Read(c)
	if e != nil || m.Kind != "prepared" {
		switch m.Kind {
		case "install", "version", "--version", "info", "run":
			var code int
			if json.Unmarshal(m.Data, &code) == nil && code >= -1 && code <= 255 {
				mark("guest-" + m.Kind + "-exit-" + strconv.Itoa(code))
			}
		}
		// A guest that rejects preparation closes vsock before syncing its
		// filesystem and powering off. Let that shutdown finish on its own.
		c.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return ErrRunner
	}
	var proof guest.Proof
	if json.Unmarshal(m.Data, &proof) != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	mark("proof-received")
	<-done
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		return ErrRunner
	}
	if state.Atomic(filepath.Join(dir, "proof.json"), proof, nil) != nil {
		return ErrRunner
	}
	_ = os.Remove(filepath.Join(dir, "plan.json"))
	return nil
}

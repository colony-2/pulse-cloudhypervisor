package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	var plan guest.Preparation
	if state.Read(filepath.Join(dir, "plan.json"), &plan) != nil || plan.Identity != s.Identity {
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
	args := Args(dir, s)
	for i, v := range args {
		if v == "--cmdline" {
			args[i+1] = strings.ReplaceAll(args[i+1], "pulse.launch=prepare", "pulse.prepare="+s.Identity)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Hypervisor, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if cmd.Start() != nil {
		return ErrRunner
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	c, e := connectGuest(filepath.Join(dir, "vsock"), done)
	if e != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(29 * time.Minute))
	data, _ := json.Marshal(plan)
	if protocol.Write(c, protocol.Message{Kind: "prepare", Data: data}) != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	m, e := protocol.Read(c)
	if e != nil || m.Kind != "prepared" {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
	var proof guest.Proof
	if json.Unmarshal(m.Data, &proof) != nil {
		cmd.Process.Kill()
		<-done
		return ErrRunner
	}
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

package cloudhypervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	"golang.org/x/sys/unix"
)

// A KVM device is sufficient to host ordinary workers. ARM EL2 is the
// additional host extension needed to expose KVM inside one of those workers.
func nestedBootID() (string, error) {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	api, err := unix.IoctlRetInt(int(f.Fd()), 0xae00) // KVM_GET_API_VERSION
	if err != nil {
		return "", fmt.Errorf("KVM_GET_API_VERSION: %w", err)
	}
	if api != 12 {
		return "", fmt.Errorf("KVM API version %d, expected 12", api)
	}
	switch runtime.GOARCH {
	case "arm64":
		const kvmCheckExtension, kvmCapArmEL2 = 0xae03, 240
		value, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), kvmCheckExtension, kvmCapArmEL2)
		if errno != 0 {
			return "", fmt.Errorf("KVM_CAP_ARM_EL2: %w", errno)
		}
		if value == 0 {
			return "", errors.New("host KVM does not expose KVM_CAP_ARM_EL2")
		}
	case "amd64":
		if err := x86NestedSetting(os.ReadFile); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported host architecture %s", runtime.GOARCH)
	}
	return runner.BootID()
}

// Sysfs can be hidden from the pool account. In that case setup tries the
// disposable guest probe, which must still succeed before KVM is advertised.
func x86NestedSetting(read func(string) ([]byte, error)) error {
	var disabled error
	for _, module := range []string{"kvm_intel", "kvm_amd"} {
		path := filepath.Join("/sys/module", module, "parameters/nested")
		data, err := read(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(data))
		switch strings.ToUpper(value) {
		case "1", "Y":
			return nil
		case "0", "N":
			disabled = fmt.Errorf("%s=%s disables guest nesting; enable nesting for %s on this host", path, value, module)
		}
	}
	return disabled
}

// This evidence expires on host reboot or changes to the source or VM profile.
type kvmBinding struct {
	Version                               int
	Boot, Source, Hypervisor, Fingerprint string
	VCPUs                                 int
	MemoryBytes                           int64
	GICTraps                              bool
}

func (p *Runtime) kvmBinding(b Base) (kvmBinding, error) {
	boot, err := p.nestedBoot()
	if err != nil {
		return kvmBinding{}, err
	}
	if boot == "" {
		return kvmBinding{}, errors.New("host boot identity unavailable")
	}
	src, err := p.sourceProof(b)
	if err != nil {
		return kvmBinding{}, fmt.Errorf("prepared source proof: %w", err)
	}
	if !src.Proof.SupportsKVMProbe {
		return kvmBinding{}, errors.New("prepared guest agent does not support the KVM probe")
	}
	return kvmBinding{Version: 1, Boot: boot, Source: src.Digest, Hypervisor: p.config.Hypervisor.SHA256,
		Fingerprint: src.Fingerprint, VCPUs: p.config.VCPUs, MemoryBytes: p.config.MemoryBytes, GICTraps: runtime.GOARCH == "arm64" && p.config.KVMGICTraps}, nil
}
func (p *Runtime) kvmProofPath(b Base) string {
	return filepath.Join(p.config.StateDir, "sources", Fingerprint(b)+".kvm.json")
}
func (p *Runtime) verifyKVMProof(b Base, want kvmBinding) error {
	path := p.kvmProofPath(b)
	var saved kvmBinding
	if err := state.Read(path, &saved); err != nil {
		return fmt.Errorf("read guest KVM proof %s: %w", path, err)
	}
	if saved != want {
		return fmt.Errorf("guest KVM proof %s differs from current host or VM profile: saved %#v, current %#v", path, saved, want)
	}
	return nil
}
func (p *Runtime) capabilities(b Base, includeKVM bool) []string {
	caps := make([]string, 0, len(b.Capabilities)+1)
	for _, cap := range b.Capabilities {
		if cap != "kvm" {
			caps = append(caps, cap)
		}
	}
	if includeKVM {
		binding, err := p.kvmBinding(b)
		if err == nil {
			err = p.verifyKVMProof(b, binding)
		}
		if err != nil {
			fmt.Fprintf(p.setupOutput, "Catalog omitted KVM for %s: %v\n", b.Name, err)
		} else {
			caps = append(caps, "kvm")
		}
	}
	return caps
}

package cloudhypervisor

import (
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
func nestedBootID() string {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	api, err := unix.IoctlRetInt(int(f.Fd()), 0xae00) // KVM_GET_API_VERSION
	if err != nil || api != 12 {
		return ""
	}
	switch runtime.GOARCH {
	case "arm64":
		const kvmCheckExtension, kvmCapArmEL2 = 0xae03, 240
		value, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), kvmCheckExtension, kvmCapArmEL2)
		if errno != 0 || value == 0 {
			return ""
		}
	case "amd64":
		enabled := false
		for _, module := range []string{"kvm_intel", "kvm_amd"} {
			value, err := os.ReadFile(filepath.Join("/sys/module", module, "parameters/nested"))
			if err == nil && (strings.TrimSpace(string(value)) == "1" || strings.EqualFold(strings.TrimSpace(string(value)), "Y")) {
				enabled = true
				break
			}
		}
		if !enabled {
			return ""
		}
	default:
		return ""
	}
	boot, _ := runner.BootID()
	return boot
}

// This evidence expires on host reboot or changes to the source or VM profile.
type kvmBinding struct {
	Version                               int
	Boot, Source, Hypervisor, Fingerprint string
	VCPUs                                 int
	MemoryBytes                           int64
	GICTraps                              bool
}

func (p *Runtime) kvmBinding(b Base) (kvmBinding, bool) {
	boot := p.nestedBoot()
	if boot == "" {
		return kvmBinding{}, false
	}
	src, err := p.sourceProof(b)
	if err != nil || !src.Proof.SupportsKVMProbe {
		return kvmBinding{}, false
	}
	return kvmBinding{Version: 1, Boot: boot, Source: src.Digest, Hypervisor: p.config.Hypervisor.SHA256,
		Fingerprint: src.Fingerprint, VCPUs: p.config.VCPUs, MemoryBytes: p.config.MemoryBytes, GICTraps: runtime.GOARCH == "arm64" && p.config.KVMGICTraps}, true
}
func (p *Runtime) kvmProofPath(b Base) string {
	return filepath.Join(p.config.StateDir, "sources", Fingerprint(b)+".kvm.json")
}
func (p *Runtime) hasKVMProof(b Base, want kvmBinding) bool {
	var saved kvmBinding
	return state.Read(p.kvmProofPath(b), &saved) == nil && saved == want
}
func (p *Runtime) capabilities(b Base, includeKVM bool) []string {
	caps := make([]string, 0, len(b.Capabilities)+1)
	for _, cap := range b.Capabilities {
		if cap != "kvm" {
			caps = append(caps, cap)
		}
	}
	if includeKVM {
		if binding, ok := p.kvmBinding(b); ok && p.hasKVMProof(b, binding) {
			caps = append(caps, "kvm")
		}
	}
	return caps
}

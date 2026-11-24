// Package runner implements an independently supervised, one-attempt VM runner.
package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

var ErrRunner = errors.New("runner: dispatch or observation uncertain")

type Spec struct {
	Version          int            `json:"version"`
	Owner            string         `json:"owner"`
	LaunchID         string         `json:"launch_id"`
	Identity         string         `json:"identity"`
	Job              api.JobRef     `json:"job"`
	Unit             string         `json:"unit"`
	BootID           string         `json:"boot_id"`
	Created          time.Time      `json:"created"`
	Slot             int            `json:"slot"`
	Facts            api.Allocation `json:"facts"`
	Platform         string         `json:"platform"`
	Hypervisor       string
	HypervisorDigest string
	Kernel           string
	KernelDigest     string
	Initramfs        string
	InitramfsDigest  string
	Source           string
	SourceDigest     string
	Fingerprint      string `json:"fingerprint"`
	VCPUs            int    `json:"vcpus"`
	MemoryBytes      int64
	DiskBytes        int64
	TimeoutSeconds   int64 `json:"timeout_seconds"`
	Systemctl        string
	UserManager      bool
	TAP              string
	Address          string
	Gateway          string
	Netmask          string
	DNS              string
	MAC              string
	CID              uint32 `json:"cid"`
	NestedKVM        bool   `json:"nested_kvm,omitempty"`
	KVMGICTraps      bool   `json:"kvm_gic_traps,omitempty"`
}

type Status struct {
	Version  int       `json:"version"`
	Identity string    `json:"identity"`
	Phase    string    `json:"phase"`
	Started  time.Time `json:"started"`
}

func Digest(s Spec) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func BootID() (string, error) {
	b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", ErrRunner
	}
	return string(b), nil
}
func ReadSpec(dir string) (Spec, error) {
	var s Spec
	e := state.Read(filepath.Join(dir, "record.json"), &s)
	if e != nil || s.Version != 1 || s.Identity == "" || s.LaunchID == "" || s.Owner == "" {
		return Spec{}, ErrRunner
	}
	return s, nil
}
func ReadStatus(dir string, s Spec) (Status, error) {
	var v Status
	e := state.Read(filepath.Join(dir, "status.json"), &v)
	if e != nil || v.Version != 1 || v.Identity != s.Identity {
		return v, ErrRunner
	}
	return v, nil
}

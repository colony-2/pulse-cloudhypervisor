package guest

import (
	"context"
	_ "embed"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"golang.org/x/sys/unix"
)

//go:embed kvm_setup.py
var nestedSetup string

const nestedProbe = `import fcntl, os, socket, json, subprocess
with open("/dev/kvm", "r+b", buffering=0) as fd:
    if fcntl.ioctl(fd, 0xae00, 0) != 12:
        raise RuntimeError("unexpected KVM API")
    vm = fcntl.ioctl(fd, 0xae01, 0)  # KVM_CREATE_VM
    try:
        target = bytearray(32)       # struct kvm_vcpu_init
        fcntl.ioctl(vm, 0x8020aeaf, target)  # KVM_ARM_PREFERRED_TARGET
        cpu = fcntl.ioctl(vm, 0xae41, 0)     # KVM_CREATE_VCPU
        try:
            fcntl.ioctl(cpu, 0x4020aeae, target)  # KVM_ARM_VCPU_INIT
        finally:
            os.close(cpu)
    finally:
        os.close(vm)
with open(os.environ["PULSE_KVM_NETWORK"]) as f:
    network = json.load(f)
with socket.create_connection((network["gateway"], 53), timeout=3):
    pass
tap = ["ip", "tuntap"]
subprocess.run(tap + ["add", "dev", "pulse-probe", "mode", "tap", "vnet_hdr"], check=True)
try:
    subprocess.run(["ip", "link", "set", "pulse-probe", "master", network["bridge"], "up"], check=True)
finally:
    subprocess.run(tap + ["del", "dev", "pulse-probe", "mode", "tap"], check=True)
`

func nestedWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr.AmbientCaps = []uintptr{unix.CAP_NET_ADMIN}
	cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "PULSE_KVM_NETWORK=/run/pulse-kvm/network.json")
}

func setupKVM(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	const path = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	setup := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-c", nestedSetup)
	setup.Env = []string{path, "HOME=/root"}
	if setup.Run() != nil {
		return ErrAgent
	}
	// Restarting resolved to add the gateway listener drops its runtime link settings.
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil || Network(ctx, string(cmdline)) != nil {
		return ErrAgent
	}
	for _, args := range [][]string{
		{"/usr/bin/python3", "-I", "-c", nestedProbe},
		{"/usr/bin/systemd-run", "--user", "--wait", "--collect", "--pipe", "--quiet",
			"--setenv=PULSE_KVM_NETWORK=/run/pulse-kvm/network.json", "--setenv=" + path,
			"/usr/local/bin/pulse-kvm-run", "/usr/bin/python3", "-I", "-c", nestedProbe},
	} {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env = []string{path, "HOME=/home/worker"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
		nestedWorker(cmd)
		if cmd.Run() != nil {
			return ErrAgent
		}
	}
	return nil
}

// ProbeKVM runs on a disposable clone of the already-prepared disk. It never
// receives a lease or installs a preparation recipe.
func ProbeKVM(ctx context.Context, c net.Conn, identity string) error {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	msg, err := protocol.Read(c)
	var plan Preparation
	if err != nil || msg.Kind != "prepare" || json.Unmarshal(msg.Data, &plan) != nil || !plan.ProbeKVM || plan.Identity != identity || identity == "" {
		return ErrAgent
	}
	if err = setupKVM(ctx); err != nil {
		return err
	}
	data, _ := json.Marshal(Proof{KVMIdentity: identity})
	return protocol.Write(c, protocol.Message{Kind: "prepared", Data: data})
}

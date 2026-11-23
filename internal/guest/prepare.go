package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
)

// Preparation is used only in a disposable builder, before a reusable stopped
// source is published. The content-pinned recipe installs guest tools only.
type Preparation struct {
	Identity, Recipe, Script string
	DockerImage              string
	Tools                    map[string]string
}
type Proof struct {
	DockerImage                  string
	Ubuntu, Architecture, Recipe string
	Tools                        map[string]string
	Docker                       bool
	CPUMillis                    int64
	MemoryBytes                  int64
}

func Prepare(ctx context.Context, c net.Conn, identity string) error {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Minute))
	m, e := protocol.Read(c)
	if e != nil || m.Kind != "prepare" {
		return ErrAgent
	}
	var p Preparation
	if json.Unmarshal(m.Data, &p) != nil || p.Identity != identity {
		return ErrAgent
	}
	h := sha256.Sum256([]byte(p.Script))
	if hex.EncodeToString(h[:]) != p.Recipe {
		return ErrAgent
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-eu")
	cmd.Stdin = strings.NewReader(p.Script)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = []string{"HOME=/root", "PATH=/usr/local/bin:/usr/bin:/bin:/nix/var/nix/profiles/default/bin"}
	if cmd.Run() != nil {
		return ErrAgent
	}
	osrel, e := os.ReadFile("/etc/os-release")
	if e != nil || !strings.Contains(string(osrel), "ID=ubuntu\n") || !strings.Contains(string(osrel), "VERSION_ID=\"24.04\"") {
		return ErrAgent
	}
	// Measure installed executable bytes, independently of recipe metadata.
	paths := map[string]string{"c2j": "/usr/local/bin/c2j", "nix": "/nix/var/nix/profiles/default/bin/nix", "docker": "/usr/bin/docker", "guest-agent": "/usr/local/bin/pulse-cloudhypervisor-guest"}
	tools := map[string]string{}
	for name, path := range paths {
		f, e := os.Open(path)
		if e != nil {
			return ErrAgent
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return ErrAgent
		}
		tools[name] = hex.EncodeToString(h.Sum(nil))
		if tools[name] != p.Tools[name] {
			return ErrAgent
		}
	}

	for _, args := range [][]string{
		{"/usr/local/bin/c2j", "version"},
		{"/nix/var/nix/profiles/default/bin/nix", "--version"},
		{"/usr/bin/docker", "info"},
		{"/usr/bin/docker", "run", "--pull=never", "--rm", "--network=none", "--user=1000:1000", p.DockerImage, "/bin/true"},
	} {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
		cmd.Env = []string{"HOME=/home/worker", "PATH=/usr/local/bin:/usr/bin:/bin:/nix/var/nix/profiles/default/bin"}
		cmd.Dir = "/home/worker"
		cmd.Stdout = nil
		cmd.Stderr = nil
		if cmd.Run() != nil {
			return ErrAgent
		}
	}
	// Prepared sources must not retain executed identity or worker intent.
	if _, e := os.Lstat("/var/lib/pulse-guest/worker-intent"); !os.IsNotExist(e) {
		return ErrAgent
	}
	if os.WriteFile("/etc/machine-id", nil, 0644) != nil {
		return ErrAgent
	}
	_ = os.Remove("/var/lib/dbus/machine-id")
	mem, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return ErrAgent
	}
	var memory int64
	for _, line := range strings.Split(string(mem), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" {
			n, e := strconv.ParseInt(fields[1], 10, 64)
			if e != nil {
				return ErrAgent
			}
			memory = n * 1024
		}
	}
	// Reserve guest OS overhead; never report nominal configured VM memory.
	memory -= 512 << 20
	if memory <= 0 {
		return ErrAgent
	}
	proof := Proof{Ubuntu: "24.04", Architecture: runtime.GOARCH, Recipe: p.Recipe, Tools: tools, Docker: true, DockerImage: p.DockerImage, CPUMillis: int64(runtime.NumCPU()) * 1000, MemoryBytes: memory}
	b, _ := json.Marshal(proof)
	return protocol.Write(c, protocol.Message{Kind: "prepared", Data: b})
}

// Run as the prepared guest service, once per cold boot. No host filesystem
// mounts or cloud-init credential data are used.
package main

import (
	"context"
	"os"
	"strings"
	"syscall"

	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/mdlayher/vsock"
)

func main() {
	b, e := os.ReadFile("/proc/cmdline")
	if e != nil {
		os.Exit(1)
	}
	prepareID := ""
	cfg := guest.Config{IntentDir: "/var/lib/pulse-guest", C2J: "/usr/local/bin/c2j", UID: 1000, GID: 1000}
	for _, part := range strings.Fields(string(b)) {
		if strings.HasPrefix(part, "pulse.prepare=") {
			prepareID = strings.TrimPrefix(part, "pulse.prepare=")
		}
		if strings.HasPrefix(part, "pulse.launch=") {
			cfg.LaunchID = strings.TrimPrefix(part, "pulse.launch=")
		}
		if strings.HasPrefix(part, "pulse.identity=") {
			cfg.Identity = strings.TrimPrefix(part, "pulse.identity=")
		}
	}
	if prepareID == "" && (cfg.LaunchID == "" || cfg.Identity == "") {
		os.Exit(1)
	}
	if guest.Network(context.Background(), string(b)) != nil {
		os.Exit(1)
	}
	l, e := vsock.Listen(protocol.Port, nil)
	if e != nil {
		os.Exit(1)
	}
	defer l.Close()
	c, e := l.Accept()
	if e != nil {
		os.Exit(1)
	}
	// A guest exits after the one delivery attempt, including invalid or uncertain
	// delivery. A fresh disk/identity is required for a later authorized launch.
	if prepareID != "" {
		_ = guest.Prepare(context.Background(), c, prepareID)
	} else {
		_ = guest.Serve(context.Background(), c, cfg)
	}
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ArmBackstop confirms the capped service is active before VMM launch.
// systemd starts RuntimeMaxSec when a Type=notify service becomes active. The
// runner stays in its bounded starting phase while it verifies and clones the
// disk. Only this process can announce readiness to its supervising manager.
func ArmBackstop(s Spec) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if s.Systemctl == "" || s.Unit == "" || s.TimeoutSeconds <= 0 ||
		!(filepath.IsAbs(socket) || strings.HasPrefix(socket, "@")) {
		return ErrRunner
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return ErrRunner
	}
	_, err = conn.Write([]byte("READY=1"))
	conn.Close()
	if err != nil {
		return ErrRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := []string{}
	if s.UserManager {
		args = append(args, "--user")
	}
	show := append(append([]string{}, args...), "show", s.Unit, "--property=Description,ActiveState,MainPID")
	// busctl exposes the D-Bus integer in microseconds. systemctl show renders
	// longer durations for humans and can round values in that representation.
	bus := filepath.Join(filepath.Dir(s.Systemctl), "busctl")
	path := "/org/freedesktop/systemd1/unit/" + strings.NewReplacer("-", "_2d", ".", "_2e").Replace(s.Unit)
	get := append(append([]string{}, args...), "get-property", "org.freedesktop.systemd1", path,
		"org.freedesktop.systemd1.Service", "RuntimeMaxUSec")
	value, err := exec.CommandContext(ctx, bus, get...).Output()
	if err != nil || strings.TrimSpace(string(value)) != fmt.Sprintf("t %d", s.TimeoutSeconds*1000000) {
		return ErrRunner
	}
	for ctx.Err() == nil {
		out, err := exec.CommandContext(ctx, s.Systemctl, show...).Output()
		if err != nil {
			return ErrRunner
		}
		fields := make(map[string]string)
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				fields[k] = v
			}
		}
		if fields["Description"] != "pulse:"+s.Identity || fields["MainPID"] != strconv.Itoa(os.Getpid()) {
			return ErrRunner
		}
		if fields["ActiveState"] == "active" {
			return nil
		}
		if fields["ActiveState"] != "activating" {
			return ErrRunner
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ErrRunner
}

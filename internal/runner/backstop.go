package runner

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The unit starts with a bounded preparation allowance. Immediately before the
// single native dispatch, shorten its independent lifetime to the runtime cap
// relative to the manager's monotonic start time. Failure permits no VM start.
func armBackstop(s Spec) error {
	if s.Systemctl == "" || s.Unit == "" {
		return ErrRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{}
	if s.UserManager {
		args = append(args, "--user")
	}
	show := append(append([]string{}, args...), "show", s.Unit, "--property=ActiveEnterTimestampMonotonic", "--value")
	b, e := exec.CommandContext(ctx, s.Systemctl, show...).Output()
	if e != nil {
		return ErrRunner
	}
	start, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if e != nil || start <= 0 {
		return ErrRunner
	}
	var now unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &now) != nil {
		return ErrRunner
	}
	elapsed := now.Nano()/1000 - start
	if elapsed < 0 {
		return ErrRunner
	}
	limit := elapsed + s.TimeoutSeconds*1000000
	set := append(args, "set-property", "--runtime", s.Unit, "RuntimeMaxSec="+strconv.FormatInt(limit, 10)+"us")
	if exec.CommandContext(ctx, s.Systemctl, set...).Run() != nil {
		return ErrRunner
	}
	return nil
}

//go:build live

package live

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
	"golang.org/x/sys/unix"
)

func TestS23(t *testing.T) {
	for _, mode := range []string{"cancel", "stall"} {
		t.Run(mode, func(t *testing.T) {
			e := openExecution(t)
			l, _ := e.launch(t, "logs-"+mode, "valid", "output")
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			received := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			var first sync.Once
			sub := e.submit(t, l, api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx,
				OnLog: func(_ context.Context, event api.LaunchLog) error {
					if event.LaunchID == l.LaunchID && event.Sequence > 0 && len(event.Data) > 0 {
						first.Do(func() { close(received) })
						<-release
					}
					return nil
				},
			}})
			select {
			case <-received:
			case <-e.ctx.Done():
				t.Fatal("native listener received no correlated output")
			}
			expected := api.ListenerSlow
			cause := api.ErrLogDelivery
			if mode == "cancel" {
				expected = api.ListenerContextCanceled
				cause = context.Canceled
				cancel()
			}
			result, err := sub.Wait(e.ctx)
			if result.Reason != expected || !errors.Is(err, cause) || !errors.Is(result.Cause, cause) {
				t.Fatal("listener loss did not terminate with its own reason")
			}
			e.waitForCleanup(t, l)
			status, err := e.observe(e.ctx, l.LaunchID)
			if err != nil || status.Job != l.Job || status.Acquisitions != 1 || status.ClaimAttempts != 0 ||
				!status.OutputOK || status.Outcome != "success" {
				t.Logf("JobDB state %s, outcome %s, acquisitions %d, renewals %d, output verified %t", status.State, status.Outcome, status.Acquisitions, status.Renewals, status.OutputOK)
				t.Fatal("job lost or truncated stdout or stderr after listener loss")
			}
		})
	}
	t.Run("sdk-loss", sdkTransportLoss)
}

func sdkTransportLoss(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "sdk-interrupted", "valid", "sdk-output")
	if e.provider.Close() != nil {
		t.Fatal("release owner for the SDK host")
	}
	bin := os.Getenv("CLOUDHYPERVISOR_PROVIDER_BINARY")
	if !filepath.IsAbs(bin) {
		t.Fatal("candidate SDK executable unavailable")
	}
	pidFile := filepath.Join(t.TempDir(), "sdk-pid")
	config, err := json.Marshal(e.config)
	if err != nil {
		t.Fatal("prepare SDK initialization")
	}
	// The private wrapper records its own PID before exec. Only the process
	// hosting this SDK transport receives a signal. Its runner is independent.
	client, err := plugin.Start(e.ctx, plugin.StartOptions{
		Executable: "/bin/sh", Args: []string{"-c", `umask 077; printf '%s\n' "$$" > "$1"; exec "$2" server`,
			"sdk-test", pidFile, bin}, Init: plugin.Initialization{Name: "native", BaseDir: "/tmp", ConfigYAML: config},
		Env: map[string]string{"PATH": "/usr/bin:/bin", "XDG_RUNTIME_DIR": os.Getenv("XDG_RUNTIME_DIR"),
			"DBUS_SESSION_BUS_ADDRESS": os.Getenv("DBUS_SESSION_BUS_ADDRESS"), "HOME": filepath.Dir(pidFile)},
		StartupTimeout: 15 * time.Second, CallTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal("start real SDK host")
	}
	defer client.Close()
	caps, err := client.GetCapabilities(e.ctx)
	if err != nil || len(caps.Bases) != 1 || client.Health().Protocol != 3 {
		t.Fatal("qualify native SDK owner")
	}
	out := new(guestOutput)
	results, sub, err := client.Submit(e.ctx, []api.Launch{l}, out.listen(e.ctx, l.LaunchID))
	if err != nil || len(results) != 1 || results[0].Status != api.Accepted || results[0].LaunchID != l.LaunchID || sub == nil {
		t.Fatal("dispatch through the SDK transport")
	}
	defer sub.Stop()
	e.waitForWorker(t, out)
	pidText, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal("SDK process identity unavailable")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid SDK process identity")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal("open SDK pidfd")
	}
	defer unix.Close(fd)
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	exe, err := os.Readlink(filepath.Join(proc, "exe"))
	argv, argsErr := os.ReadFile(filepath.Join(proc, "cmdline"))
	stat, statErr := os.Stat(proc)
	if err != nil || exe != bin || argsErr != nil || string(argv) != bin+"\x00server\x00" || statErr != nil ||
		stat.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		t.Fatal("SDK process changed identity")
	}
	if unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) != nil {
		t.Fatal("interrupt test-owned SDK transport")
	}
	ending, _ := sub.Wait(e.ctx)
	if ending.Reason != api.ListenerTransportLost {
		t.Fatal("SDK listener did not report transport loss")
	}
	_ = client.Close()
	e.reopen(t)
	page, err := e.provider.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
		t.Fatal("recover job after SDK loss")
	}
	e.waitForCleanup(t, l)
	status, err := e.observe(e.ctx, l.LaunchID)
	if err != nil || status.Job != l.Job || status.Acquisitions != 1 || status.ClaimAttempts != 0 || !status.RenewedBeyondInitial ||
		!status.OutputOK || status.Outcome != "success" {
		t.Fatal("authoritative output or lease lost after SDK termination")
	}
}

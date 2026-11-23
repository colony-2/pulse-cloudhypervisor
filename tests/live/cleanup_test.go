//go:build live

package live

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	api "github.com/colony-2/pulse-provider-api"
)

func TestS22(t *testing.T) {
	e := openExecution(t)
	l, _ := e.launch(t, "quarantine-running", "valid", "lifetime")
	out := new(guestOutput)
	e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	e.waitForWorker(t, out)
	dir := launchDirectory(e.config.StateDir, l.LaunchID)
	spec, err := runner.ReadSpec(dir)
	if err != nil || spec.Job != l.Job || spec.Owner != e.config.Owner {
		t.Fatal("identify active fault target")
	}
	e.ownedPair(t, dir, spec)
	disk, err := os.Stat(filepath.Join(dir, "disk.raw"))
	if err != nil {
		t.Fatal("active disk missing")
	}
	fence, err := os.Stat(filepath.Join(dir, "start.fence"))
	if err != nil {
		t.Fatal("active start fence missing")
	}
	canary := filepath.Join(t.TempDir(), "foreign-state")
	if os.WriteFile(canary, []byte("unrelated"), 0600) != nil {
		t.Fatal("foreign canary creation")
	}
	waiting, _ := e.launch(t, "quarantine-second", "valid", "renewal")
	if e.provider.Close() != nil {
		t.Fatal("close original controller")
	}

	// Fault only the newly opened controller's observer. The real runner, unit,
	// workload disk and JobDB helper remain live throughout the observations.
	for _, fault := range []string{"unreachable", "foreign", "stale-pid"} {
		t.Run(fault, func(t *testing.T) {
			control := filepath.Join(t.TempDir(), "systemctl")
			var body string
			switch fault {
			case "unreachable":
				body = "#!/usr/bin/python3 -I\nimport socket\nwith socket.socket(socket.AF_UNIX) as s:\n s.connect(__file__+'.absent')\n"
			case "foreign":
				body = "#!/bin/sh\nprintf '%s\\n' 'Description=pulse:foreign' 'ActiveState=inactive' 'SubState=dead' 'ControlGroup='\n"
			case "stale-pid":
				// Even apparent terminal status cannot release the runner's held lock.
				body = "#!/bin/sh\nprintf '%s\\n' 'Description=pulse:" + spec.Identity + "' 'MainPID=1' 'ActiveState=inactive' 'SubState=dead' 'ControlGroup='\n"
			}
			if os.WriteFile(control, []byte(body), 0700) != nil {
				t.Fatal("create private observation fault")
			}
			cfg := e.config
			slots := 1
			cfg.Count = &slots
			cfg.Systemctl = control
			controller, err := ch.New(cfg)
			if err != nil {
				t.Fatal("recover faulted owner")
			}
			defer controller.Close()
			if err := controller.Reconcile(e.ctx); err == nil {
				t.Fatal("uncertain native observation accepted")
			}
			page, err := controller.List(e.ctx, api.ListRequest{LaunchID: l.LaunchID})
			if err != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
				t.Fatal("lost active job under observation fault")
			}
			results, sub, err := controller.Submit(e.ctx, []api.Launch{waiting}, api.SubmitOptions{})
			if err != nil || sub != nil || len(results) != 1 || results[0].Status != api.NoCapacity {
				t.Fatal("uncertain guest lost its reservation")
			}
			diskAfter, diskErr := os.Stat(filepath.Join(dir, "disk.raw"))
			fenceAfter, fenceErr := os.Stat(filepath.Join(dir, "start.fence"))
			marker, readErr := os.ReadFile(canary)
			if diskErr != nil || fenceErr != nil || !os.SameFile(disk, diskAfter) || !os.SameFile(fence, fenceAfter) ||
				readErr != nil || !bytes.Equal(marker, []byte("unrelated")) {
				t.Fatal("uncertain or foreign resources changed")
			}
		})
	}
	e.reopen(t)
	e.waitForCleanup(t, l)
	status, err := e.observe(e.ctx, l.LaunchID)
	if err != nil || status.Job != l.Job || status.Acquisitions != 1 || status.ClaimAttempts != 0 || !status.OutputOK || status.Outcome != "success" {
		t.Fatal("original job failed to finish after restoring observation")
	}
}

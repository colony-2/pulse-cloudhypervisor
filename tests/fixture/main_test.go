package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
)

func setup(t *testing.T) (*fixture, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f, err := newFixture(ctx, "127.0.0.1:0", "http://127.0.0.1:0", "test-fixture", 2*time.Second)
	if err != nil {
		t.Fatal("isolated fixture startup failed")
	}
	t.Cleanup(f.Close)
	return f, ctx
}

func receiver(t *testing.T, f *fixture) *remote.Runtime {
	t.Helper()
	r, err := remote.New(f.endpoint, &http.Client{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal("receiver startup failed")
	}
	return r
}

func TestLeaseVariantsAndHandoff(t *testing.T) {
	f, ctx := setup(t)
	r := receiver(t, f)
	for _, variant := range []string{"expired", "completed", "mismatched", "valid"} {
		t.Run(variant, func(t *testing.T) {
			a, err := f.launch(ctx, variant, variant)
			if err != nil || a.Lease == "" {
				t.Fatal("assignment unavailable")
			}
			again, err := f.launch(ctx, variant, variant)
			if err != nil || again != a {
				t.Fatal("retry changed assignment")
			}
			if _, err := f.launch(ctx, variant, "other"); err == nil {
				t.Fatal("changed variant accepted")
			}
			if strings.Contains(fmt.Sprintf("%v %#v", a, a), a.Lease) {
				t.Fatal("credential formatting exposed")
			}
			cap, err := remote.DecodeLeaseCapability([]byte(a.Lease))
			if err != nil {
				t.Fatal("invalid exported capability")
			}
			lease, err := r.ImportLease(ctx, cap)
			if variant == "expired" || variant == "completed" {
				if err == nil {
					t.Fatal("inactive lease imported")
				}
				return
			}
			if err != nil {
				t.Fatal("live lease rejected")
			}
			key := lease.Job().JobKey
			if (key.JobId == a.Job.JobID) != (variant == "valid") || key.TenantId != a.Job.TenantID {
				t.Fatal("assignment identity incorrect")
			}
			initial, err := f.status(ctx, variant)
			if err != nil || initial.Acquisitions != 1 || initial.Renewals == 0 || initial.OutputOK {
				t.Fatal("authoritative lease events unavailable")
			}
			// Only the receiver renews. It must remain valid after the handoff expiry.
			deadline := initial.InitialExpiry.Add(100 * time.Millisecond)
			for time.Now().Before(deadline) {
				time.Sleep(200 * time.Millisecond)
				lease, err = lease.Renew(ctx)
				if err != nil {
					t.Fatal("receiver renewal failed")
				}
			}
			final, err := f.status(ctx, variant)
			if err != nil || !final.RenewedBeyondInitial || !final.LatestExpiry.After(final.InitialExpiry) || final.Acquisitions != 1 || final.ClaimAttempts != 0 {
				t.Fatal("handoff renewal not established")
			}
			data, err := json.Marshal(final)
			if err != nil || bytes.Contains(data, []byte(a.Lease)) || bytes.Contains(data, []byte("leaseToken")) {
				t.Fatal("status exposed credential")
			}
		})
	}
	for _, name := range []string{"", "../escape", strings.Repeat("a", 65)} {
		if _, err := f.launch(ctx, name, "valid"); err == nil {
			t.Fatal("invalid launch name accepted")
		}
	}
	if _, err := f.status(ctx, "absent"); err == nil {
		t.Fatal("absent case accepted")
	}
}

func TestConcurrentRetryDoesNotAcquireAgain(t *testing.T) {
	f, ctx := setup(t)
	const count = 8
	assignments := make([]assignment, count)
	failures := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) { defer wg.Done(); assignments[i], failures[i] = f.launch(ctx, "retry", "valid") }(i)
	}
	wg.Wait()
	for i := range count {
		if failures[i] != nil || assignments[i] != assignments[0] {
			t.Fatal("concurrent retry changed assignment")
		}
	}
	s, err := f.status(ctx, "retry")
	if err != nil || s.Acquisitions != 1 || s.Renewals != 0 || s.OutputOK {
		t.Fatal("owner renewed or reacquired after handoff")
	}
}

func controlClient(path string) *http.Client {
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

func TestPrivateControlAndGuestBoundary(t *testing.T) {
	f, ctx := setup(t)
	path := filepath.Join(f.dir, "control")
	if err := f.serveControl(path); err != nil {
		t.Fatal("control startup failed")
	}
	client := controlClient(path)
	defer client.CloseIdleConnections()
	get := func(target, identity string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://fixture"+target, nil)
		req.Header.Set("X-Pulse-Fixture", identity)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("control request failed")
		}
		defer response.Body.Close()
		b, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("unsafe control response")
		}
		return response.StatusCode, b
	}
	for _, identity := range []string{"", "wrong"} {
		code, _ := get("/launch?name=private&case=valid", identity)
		if code != http.StatusForbidden {
			t.Fatal("foreign identity accepted")
		}
	}
	if code, _ := get("/health", f.identity); code != http.StatusOK {
		t.Fatal("health failed")
	}
	code, b := get("/launch?name=private&case=valid", f.identity)
	var a assignment
	if code != http.StatusOK || json.Unmarshal(b, &a) != nil || a.Lease == "" {
		t.Fatal("private assignment failed")
	}
	if code, _ := get("/status?name=private", f.identity); code != http.StatusOK {
		t.Fatal("status failed")
	}
	for _, target := range []string{"/v1/jobs/poll", "/v1/tenants/" + tenant + "/jobs/any/lease"} {
		req, _ := http.NewRequestWithContext(ctx, "POST", f.endpoint+target, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("guest boundary unavailable")
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal("replacement claim allowed")
		}
	}
	// Guest metadata reads are isolated; administrative mutations are denied.
	r := receiver(t, f)
	if _, err := r.CreateCell(ctx, jobdb.CreateCellRequest{TenantID: tenant, Name: "unowned", Repo: sourceRepo}); err == nil {
		t.Fatal("guest inherited administrative authority")
	}
	s, err := f.status(ctx, "private")
	if err != nil || s.ClaimAttempts != 2 || s.Acquisitions != 1 {
		t.Fatal("claim rejection not recorded")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("control socket permissions")
	}
	dir := f.dir
	f.Close()
	f.Close()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("fixture resources remain")
	}
}

func TestControlRejectsUnprotectedParent(t *testing.T) {
	f, _ := setup(t)
	dir := t.TempDir()
	if os.Chmod(dir, 0755) != nil {
		t.Fatal("permissions")
	}
	if f.serveControl(filepath.Join(dir, "socket")) == nil {
		t.Fatal("public control parent accepted")
	}
}

func TestC2JSuppliedLease(t *testing.T) {
	f, ctx := setup(t)
	a, err := f.launch(ctx, "c2j", "valid")
	if err != nil {
		t.Fatal("c2j assignment unavailable")
	}
	if err = runC2J(t, ctx, a); err != nil {
		t.Fatal("isolated c2j supplied-lease execution failed")
	}
	s, err := f.status(ctx, "c2j")
	if err != nil || s.State != "COMPLETED" || s.Outcome != "success" || !s.OutputOK || s.Acquisitions != 1 || s.Renewals == 0 || !s.RenewedBeyondInitial || s.ClaimAttempts != 0 {
		t.Fatal("isolated c2j execution not established")
	}
}

func TestWorkloadLeaseCoversColdBoot(t *testing.T) {
	f, ctx := setup(t)
	f.dockerImage = "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, profile    string
		configured, want time.Duration
	}{
		{"isolate-a", "peer-isolation", 2 * time.Second, 3 * time.Minute},
		{"isolate-b", "peer-isolation", 4 * time.Minute, 4 * time.Minute},
		{"cap-runner", "cap", 45 * time.Second, 3 * time.Minute},
		{"logs-cancel", "output", 45 * time.Second, 3 * time.Minute},
		{"cold", "prepared-tools", 45 * time.Second, 3 * time.Minute},
		{"docker", "docker", 45 * time.Second, 3 * time.Minute},
		{"sized", "sized-facts", 45 * time.Second, 3 * time.Minute},
		{"renewal", "renewal", 45 * time.Second, 45 * time.Second},
		{"sdk", "sdk-output", 45 * time.Second, 45 * time.Second},
	} {
		f.leaseDuration = tc.configured
		if _, err := f.launchProfile(ctx, tc.name, "valid", tc.profile); err != nil {
			t.Fatalf("could not acquire %s assignment", tc.profile)
		}
		events, err := f.runtime.ListLeaseEvents(ctx, jobdb.ListLeaseEventsRequest{
			TenantID:   tenant,
			LeaseIDs:   []string{f.cases[tc.name].LeaseID},
			EventTypes: []jobdb.LeaseEventType{jobdb.LeaseEventAcquired},
		})
		if err != nil || len(events.Events) != 1 {
			t.Fatal("expected one lease acquisition")
		}
		event := events.Events[0]
		if got := event.LeaseExpiresAt.Sub(event.OccurredAt); got != tc.want {
			t.Fatalf("%s lease valid for %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestS26(t *testing.T) {
	t.Run("profiles", func(t *testing.T) {
		f, ctx := setup(t)
		f.dockerImage = "sha256:" + strings.Repeat("a", 64)
		receiver := receiver(t, f)
		const outputRecord = "pulse-ch-fixture-stdout-record\n"
		const errorRecord = "pulse-ch-fixture-stderr-record\n"
		stdout := strings.TrimSpace(strings.Repeat(outputRecord, 128))
		stderr := strings.TrimSpace(strings.Repeat(errorRecord, 128))
		for _, test := range []struct {
			profile, commandFragment, stdout, stderr string
		}{
			{profile: "renewal", commandFragment: "sleep 3", stdout: "cloudhypervisor-fixture-ok"},
			{profile: "isolation", commandFragment: `mkdir "$directory"`, stdout: "test-isolation"},
			{profile: "docker", commandFragment: "--pull=never", stdout: "cloudhypervisor-docker-ok"},
			{profile: "prepared-tools", commandFragment: "sha256sum --status", stdout: "cloudhypervisor-prepared-tools"},
			{profile: "lifetime", commandFragment: "sleep 18", stdout: "cloudhypervisor-lifetime-ok"},
			{profile: "ack-lifetime", commandFragment: "sleep 75", stdout: "cloudhypervisor-ack-loss-ok"},
			{profile: "output", commandFragment: `"$n" -lt 128`, stdout: stdout, stderr: stderr},
			{profile: "sdk-output", commandFragment: "sleep 8;", stdout: stdout, stderr: stderr},
		} {
			for _, correct := range []bool{true, false} {
				name := "test-" + test.profile
				if !correct {
					name += "-wrong"
				}
				assignment, err := f.launchProfile(ctx, name, "valid", test.profile)
				if err != nil {
					t.Fatalf("profile %s could not launch", test.profile)
				}
				work := f.cases[name].Workload
				if strings.Contains(work.command, "{{") {
					t.Fatalf("fixed %s workload contains a c2j template directive", test.profile)
				}
				if !strings.Contains(work.command, test.commandFragment) {
					t.Fatalf("profile %s did not select its fixed command", test.profile)
				}
				initial, err := f.status(ctx, name)
				if err != nil || initial.OutputOK {
					t.Fatalf("profile %s had output before terminal completion", test.profile)
				}
				capability, err := remote.DecodeLeaseCapability([]byte(assignment.Lease))
				if err != nil {
					t.Fatal("invalid fixture lease")
				}
				lease, err := receiver.ImportLease(ctx, capability)
				if err != nil {
					t.Fatal("fixture lease unavailable")
				}
				stdout, stderr := test.stdout, test.stderr
				if test.profile == "isolation" {
					stdout = name
				}
				if !correct {
					stderr += "unexpected"
				}
				payload, err := json.Marshal(map[string]any{"stdout": stdout, "stderr": stderr, "success": true, "exit_code": 0})
				if err != nil {
					t.Fatal("terminal fixture payload")
				}
				chapter := jobdb.Chapter{Ordinal: 1, TaskType: lease.Route().JobType, CreatedAt: time.Now(), Body: jobdb.JobAttemptOutcomeChapter{Outcome: jobdb.ApplicationOutputOutcome{Output: jobdb.ApplicationOutputBytes{Data: payload}}}}
				if err = lease.Complete(ctx, jobdb.CompleteExecutionRequest{Status: "success", Chapter: &chapter}); err != nil {
					t.Fatal("controlled job completion failed")
				}
				final, err := f.status(ctx, name)
				if err != nil || final.OutputOK != correct {
					t.Fatalf("profile %s did not distinguish command results", test.profile)
				}
			}
		}
		if _, err := f.launchProfile(ctx, "unowned", "valid", "echo unsafe"); err == nil {
			t.Fatal("arbitrary command accepted")
		}
		if _, err := f.launchProfile(ctx, "unowned", "expired", "output"); err == nil {
			t.Fatal("unexpected variant/profile pair accepted")
		}
		if _, err := selectWorkload("docker", "unowned", "ubuntu:latest", time.Second, nil); err == nil {
			t.Fatal("mutable Docker image accepted")
		}
		if _, reserved := f.cases["unowned"]; reserved {
			t.Fatal("rejected input reserved a case")
		}
	})
	t.Run("terminal", func(t *testing.T) {
		f, ctx := setup(t)
		assignment, err := f.launchProfile(ctx, "c2j-output", "valid", "output")
		if err != nil || runC2J(t, ctx, assignment) != nil {
			t.Fatal("host c2j output workload failed")
		}
		result, err := f.status(ctx, "c2j-output")
		if err != nil || result.State != "COMPLETED" || result.Outcome != "success" || !result.OutputOK || result.Acquisitions != 1 || result.ClaimAttempts != 0 {
			t.Fatal("exact host c2j command output was not established")
		}
	})
	t.Run("renewal", TestC2JSuppliedLease)
	t.Run("retry", func(t *testing.T) {
		TestConcurrentRetryDoesNotAcquireAgain(t)
		f, ctx := setup(t)
		first, err := f.launchProfile(ctx, "compare", "valid", "output")
		if err != nil {
			t.Fatal("profile assignment unavailable")
		}
		if _, err = f.launchProfile(ctx, "compare", "valid", "lifetime"); err == nil {
			t.Fatal("retry changed workload")
		}
		retry, err := f.launchProfile(ctx, "compare", "valid", "output")
		if err != nil || first != retry {
			t.Fatal("profile retry changed the original assignment")
		}
	})
	t.Run("replacement", TestPrivateControlAndGuestBoundary)
}

func runC2J(t *testing.T, ctx context.Context, a assignment) error {
	t.Helper()
	path, err := exec.LookPath("c2j")
	if err != nil {
		t.Fatal("installed c2j required")
	}
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, path, "run", "with-lease", "--health-port", "0", "--ci", "--input-mode", "fail", "--lease-file", "-", "--jobdb", a.Job.JobDB, "--job-id", a.Job.JobID)
	// Never inherit runtime credentials, the active task lease or user Git settings.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	cmd.Dir = home
	cmd.Stdin = strings.NewReader(a.Lease)
	// c2j controls its own output; discard it rather than persisting a credential.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func TestC2JRejectsInvalidAssignments(t *testing.T) {
	f, ctx := setup(t)
	for _, variant := range []string{"expired", "mismatched", "completed"} {
		t.Run(variant, func(t *testing.T) {
			a, err := f.launch(ctx, variant, variant)
			if err != nil {
				t.Fatal("invalid assignment unavailable")
			}
			key := f.cases[variant].Key
			before, err := f.runtime.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: key})
			if err != nil {
				t.Fatal("initial observation unavailable")
			}
			initial, err := f.status(ctx, variant)
			if err != nil {
				t.Fatal("initial lease accounting unavailable")
			}
			if runC2J(t, ctx, a) == nil {
				t.Fatal("invalid assignment executed")
			}
			after, err := f.runtime.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: key})
			if err != nil || len(after) != len(before) {
				t.Fatal("invalid assignment began execution")
			}
			s, err := f.status(ctx, variant)
			if err != nil || s.OutputOK || s.Acquisitions != 1 || s.ClaimAttempts != 0 || s.Renewals != initial.Renewals {
				t.Fatal("invalid assignment changed work or acquired replacement")
			}
		})
	}
}

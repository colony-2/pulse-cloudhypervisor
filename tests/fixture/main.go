// Test-only isolated JobDB/Git backend. Lease capabilities leave memory only
// through the protected same-user Unix control socket, never stdout or files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/colony-2/c2j/pkg/contextual"
	"github.com/colony-2/c2j/pkg/jobdbschema"
	"github.com/colony-2/c2j/pkg/ops"
	"github.com/colony-2/c2j/pkg/recipe"
	"github.com/colony-2/c2j/pkg/starter"
	"github.com/colony-2/c2j/pkg/worker/commandop"
	"github.com/colony-2/c2j/pkg/workflowctl"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/celltest"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/sqlite"
	jobworkflow "github.com/colony-2/jobdb/pkg/workflow"
	"golang.org/x/sys/unix"
)

const tenant = "cloudhypervisor-fixture"
const sourceRepo = "fixture.invalid/cloudhypervisor/source"

var errFixture = errors.New("private guest fixture unavailable")
var registerOps sync.Once
var caseName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type jobRef struct {
	JobDB      string `json:"jobdb"`
	InstanceID string `json:"instance_id"`
	TenantID   string `json:"tenant_id"`
	JobID      string `json:"job_id"`
}
type assignment struct {
	Job   jobRef `json:"job"`
	Lease string `json:"lease"`
}

func (assignment) String() string     { return "fixture assignment (redacted)" }
func (a assignment) GoString() string { return a.String() }

type heldCase struct {
	Assignment    assignment
	Key           jobdb.JobKey
	LeaseID       string
	Variant       string
	Profile       string
	Workload      workload
	InitialExpiry time.Time
}
type guestIdentity struct {
	Machine string `json:"machine"`
	Launch  string `json:"launch"`
	MAC     string `json:"mac"`
	Address string `json:"address"`
	CID     uint32 `json:"cid"`
}

var machineIDPattern = regexp.MustCompile("^[0-9a-f]{32}$")
var guestMACPattern = regexp.MustCompile("^02(:[0-9a-f]{2}){5}$")

func (id guestIdentity) valid(expectedLaunch string) bool {
	addr, err := netip.ParseAddr(id.Address)
	return id.Launch == expectedLaunch && machineIDPattern.MatchString(id.Machine) &&
		guestMACPattern.MatchString(id.MAC) && err == nil && addr.Is4() && id.CID >= 3
}

type status struct {
	Identity             *guestIdentity `json:"guest_identity,omitempty"`
	Job                  jobRef         `json:"job"`
	State                string         `json:"state"`
	Outcome              string         `json:"outcome"`
	OutputOK             bool           `json:"output_ok"`
	InitialExpiry        time.Time      `json:"initial_expiry"`
	LatestExpiry         time.Time      `json:"latest_expiry"`
	Acquisitions         int            `json:"acquisitions"`
	Renewals             int            `json:"renewals"`
	RenewedBeyondInitial bool           `json:"renewed_beyond_initial"`
	Chapters             int            `json:"chapters"`
	ClaimAttempts        int64          `json:"claim_attempts"`
}
type fixture struct {
	dir, endpoint, identity, hash  string
	db                             *sqlite.Runtime
	runtime                        *remote.Runtime
	engine                         jobworkflow.Engine
	guest, control                 *http.Server
	guestListener, controlListener net.Listener
	leaseDuration                  time.Duration
	dockerImage                    string
	mu                             sync.Mutex
	cases                          map[string]heldCase
	claims                         atomic.Int64
	once                           sync.Once
}

// The administrative client stays in-process. Guest requests receive only read
// access to disposable fixture metadata. The helper never opens a production
// JobDB or reads a task lease.
type localTransport struct{ handler http.Handler }

func (t localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	authority := jobdb.CellAuthority{Principal: "private-fixture", TenantID: tenant, Permissions: map[string]bool{"cells.create": true, "cells.get": true, "jobs.submit": true, "jobs.get": true, "jobs.list": true, "jobs.acquire": true}}
	t.handler.ServeHTTP(w, r.WithContext(jobdb.WithCellAuthority(r.Context(), authority)))
	return w.Result(), nil
}

func newFixture(ctx context.Context, listen, endpoint, identity string, duration time.Duration, peers ...netip.Addr) (_ *fixture, err error) {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !caseName.MatchString(identity) || duration < time.Second || duration > 10*time.Minute {
		return nil, errFixture
	}
	f := &fixture{endpoint: endpoint, identity: identity, cases: map[string]heldCase{}, leaseDuration: duration}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	f.dir, err = os.MkdirTemp("/tmp", "ch-lease-fixture-")
	if err != nil {
		return nil, errFixture
	}
	f.guestListener, err = net.Listen("tcp", listen)
	if err != nil {
		return nil, errFixture
	}
	if u.Port() == "0" {
		_, port, e := net.SplitHostPort(f.guestListener.Addr().String())
		if e != nil {
			return nil, errFixture
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
		f.endpoint = u.String()
	}
	f.db, err = sqlite.NewFromConfig(ctx, sqlite.Config{Projects: celltest.UnrestrictedProjects{}, DBPath: filepath.Join(f.dir, "jobs.db"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return nil, errFixture
	}
	api := remote.NewServer(f.db)
	f.runtime, err = remote.New(f.endpoint, &http.Client{Transport: localTransport{api}, Timeout: 10 * time.Second})
	if err != nil {
		return nil, errFixture
	}
	if _, err = f.runtime.CreateCell(ctx, jobdb.CreateCellRequest{TenantID: tenant, Name: "root", Repo: sourceRepo}); err != nil {
		return nil, errFixture
	}
	repo := filepath.Join(f.dir, "source")
	if os.MkdirAll(filepath.Join(repo, ".c2"), 0700) != nil || os.WriteFile(filepath.Join(repo, ".c2/config.yaml"), []byte("packages: []\n"), 0600) != nil {
		return nil, errFixture
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
		b, e := cmd.Output()
		return strings.TrimSpace(string(b)), e
	}
	for _, args := range [][]string{{"init", "-q", "--initial-branch=main"}, {"add", "."}, {"-c", "user.name=Private Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "isolated fixture"}} {
		if _, err = git(args...); err != nil {
			return nil, errFixture
		}
	}
	f.hash, err = git("rev-parse", "HEAD")
	if err != nil {
		return nil, errFixture
	}
	if _, err = git("clone", "-q", "--bare", repo, filepath.Join(f.dir, "repo.git")); err != nil {
		return nil, errFixture
	}
	profile, _ := json.Marshal(map[string]any{"version": 1, "entries": []any{map[string]any{"url": f.endpoint + "/git/repo.git", "rewrite_from": []string{"https://" + sourceRepo + ".git"}}}})
	if _, err = f.db.SetTenantConfigurationValue(ctx, jobdb.SetTenantConfigurationValueRequest{TenantID: tenant, Name: "_c2j_git_profile_read", Value: profile}); err != nil {
		return nil, errFixture
	}
	registerOps.Do(func() { ops.Register(commandop.GetOp()) })
	f.engine, err = jobworkflow.NewEngineBuilder().WithRuntime(f.runtime).BuildEngine()
	if err != nil {
		return nil, errFixture
	}
	gitpath, err := exec.LookPath("git")
	if err != nil {
		return nil, errFixture
	}
	gitHTTP := &cgi.Handler{Path: gitpath, Args: []string{"http-backend"}, Root: "/git", Env: []string{"GIT_PROJECT_ROOT=" + f.dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + f.dir}, Logger: log.New(io.Discard, "", 0)}
	f.guest = &http.Server{ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(peers) > 0 {
			client, err := netip.ParseAddrPort(r.RemoteAddr)
			allowed := false
			for _, peer := range peers {
				allowed = allowed || peer == client.Addr().Unmap()
			}
			if err != nil || !allowed {
				http.Error(w, "fixture network only", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/git/repo.git/") {
			if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				http.Error(w, "read-only source", http.StatusForbidden)
				return
			}
			gitHTTP.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/lease") || r.URL.Path == "/v1/jobs/poll" {
			f.claims.Add(1)
			http.Error(w, "fixture does not acquire replacement work", http.StatusForbidden)
			return
		}
		// c2j's repository/started-job readers use ordinary read APIs.
		// This grants reads of this disposable database only; all execution
		// mutations still require JobDB's capability validation.
		reader := jobdb.CellAuthority{Principal: "fixture-reader", TenantID: tenant, Permissions: map[string]bool{"cells.get": true, "jobs.get": true, "jobs.list": true}}
		api.ServeHTTP(w, r.WithContext(jobdb.WithCellAuthority(r.Context(), reader)))
	})}
	f.guest.ReadTimeout = 10 * time.Second
	f.guest.WriteTimeout = 10 * time.Second
	f.guest.IdleTimeout = time.Second
	f.guest.MaxHeaderBytes = 8192
	f.guest.Handler = http.MaxBytesHandler(f.guest.Handler, 2<<20)
	go f.guest.Serve(f.guestListener)
	return f, nil
}

func (f *fixture) launch(ctx context.Context, name, variant string) (assignment, error) {
	return f.launchProfile(ctx, name, variant, "renewal")
}

func (f *fixture) launchProfile(ctx context.Context, name, variant, profile string) (assignment, error) {
	if variant != "valid" && (profile != "renewal" || variant != "expired" && variant != "mismatched" && variant != "completed") {
		return assignment{}, errFixture
	}
	work, err := selectWorkload(profile, name, f.dockerImage, f.leaseDuration)
	if err != nil {
		return assignment{}, errFixture
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.cases[name]; ok {
		if old.Variant == variant && old.Profile == profile && old.Assignment.Lease != "" {
			return old.Assignment, nil
		}
		return assignment{}, errFixture
	}
	// Reserve before side effects. An ambiguous failure cannot create another
	// job/lease on a retry; restart of the helper discards the entire fixture.
	if len(f.cases) >= 64 {
		return assignment{}, errFixture
	}
	f.cases[name] = heldCase{}
	text := fmt.Sprintf(`id: cloudhypervisor-lease-fixture
op: command_execution
inputs:
  run: %q
  timeout: "12m"
`, work.command)
	rec, err := recipe.LoadRecipeFromString([]byte(text))
	if err != nil {
		return assignment{}, errFixture
	}
	start := func() (jobdb.JobKey, error) {
		j, e := starter.StartRecipeJob(ctx, workflowctl.StartJob{TenantId: tenant, Cell: "root", CellRepo: sourceRepo, RecipeName: rec.GetMetadata().ID, GitRef: f.hash, JobContext: contextual.JobContext{Workflow: contextual.WorkflowContext{ProjectId: tenant, CellName: "."}, GitBase: contextual.GitBaseContext{BaseRepo: sourceRepo, BaseRef: f.hash, ResolvedBaseHash: f.hash}}}, jobdbschema.WorkflowEngine{Engine: f.engine, Registry: f.runtime}, *rec)
		if e != nil {
			return jobdb.JobKey{}, errFixture
		}
		return j.Key(), nil
	}
	key, err := start()
	if err != nil {
		return assignment{}, errFixture
	}
	duration := f.leaseDuration
	if variant == "expired" {
		duration = 100 * time.Millisecond
	}
	lease, err := f.runtime.GetJobLease(ctx, jobdb.GetJobLeaseRequest{JobKey: key, WorkerID: "private-fixture-handoff", Routes: []jobdb.Route{{JobType: "recipe"}}, LeaseDuration: duration})
	if err != nil || lease == nil {
		return assignment{}, errFixture
	}
	renewable, ok := lease.(jobdb.RenewableExecutionLease)
	if !ok {
		return assignment{}, errFixture
	}
	initialExpiry := renewable.LeaseExpiry()
	capability, err := remote.ExportLease(lease)
	if err != nil {
		return assignment{}, errFixture
	}
	encoded, err := capability.Encode()
	if err != nil {
		return assignment{}, errFixture
	}
	target := key
	if variant == "mismatched" {
		target, err = start()
		if err != nil {
			return assignment{}, errFixture
		}
	}
	if variant == "completed" {
		final := jobdb.Chapter{Ordinal: 1, TaskType: lease.Route().JobType, CreatedAt: time.Now(), Body: jobdb.JobAttemptOutcomeChapter{Outcome: jobdb.ApplicationOutputOutcome{Output: jobdb.ApplicationOutputBytes{Data: []byte(`{"result":"fixture-completed"}`)}}}}
		if err = lease.Complete(ctx, jobdb.CompleteExecutionRequest{Status: "success", Chapter: &final}); err != nil {
			return assignment{}, errFixture
		}
	}
	if variant == "expired" {
		timer := time.NewTimer(time.Until(initialExpiry) + 20*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return assignment{}, errFixture
		}
	}
	a := assignment{Job: jobRef{JobDB: f.endpoint + "/" + tenant, InstanceID: f.identity, TenantID: target.TenantId, JobID: target.JobId}, Lease: string(encoded)}
	clear(encoded)
	f.cases[name] = heldCase{Assignment: a, Key: key, LeaseID: lease.LeaseID(), Variant: variant, Profile: profile, Workload: work, InitialExpiry: initialExpiry}
	return a, nil
}

func (f *fixture) status(ctx context.Context, name string) (status, error) {
	f.mu.Lock()
	entry, ok := f.cases[name]
	f.mu.Unlock()
	if !ok || entry.Assignment.Lease == "" {
		return status{}, errFixture
	}
	info, err := f.runtime.GetJob(ctx, entry.Key)
	if err != nil {
		return status{}, errFixture
	}
	s := status{Job: entry.Assignment.Job, State: string(info.Status), InitialExpiry: entry.InitialExpiry, LatestExpiry: entry.InitialExpiry, ClaimAttempts: f.claims.Load()}
	s.Job.JobID = entry.Key.JobId
	s.Outcome = info.CompletionStatus
	chapters, err := f.runtime.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: entry.Key})
	if err != nil {
		return status{}, errFixture
	}
	s.Chapters = len(chapters)
	for _, chapter := range chapters {
		if final, ok := chapter.Body.(jobdb.JobAttemptOutcomeChapter); ok {
			if output, ok := final.Outcome.(jobdb.ApplicationOutputOutcome); ok {
				var result struct {
					Stdout          string `json:"stdout"`
					Stderr          string `json:"stderr"`
					Success         bool   `json:"success"`
					ExitCode        int    `json:"exit_code"`
					StdoutTruncated bool   `json:"stdout_truncated"`
					StderrTruncated bool   `json:"stderr_truncated"`
					TimedOut        bool   `json:"timed_out"`
				}
				terminal := info.Status == jobdb.JobStatusCompleted && info.CompletionStatus == "success" && json.Unmarshal(output.Output.Data, &result) == nil && result.Success && result.ExitCode == 0 && !result.StdoutTruncated && !result.StderrTruncated && !result.TimedOut
				s.OutputOK = terminal && result.Stdout == entry.Workload.stdout && result.Stderr == entry.Workload.stderr
				if entry.Profile == "peer-isolation" && terminal && result.Stderr == "" && len(result.Stdout) < 512 {
					var id guestIdentity
					if json.Unmarshal([]byte(result.Stdout), &id) == nil && id.valid(name) {
						s.Identity, s.OutputOK = &id, true
					}
				}
			}
		}
	}
	page := ""
	for {
		events, e := f.runtime.ListLeaseEvents(ctx, jobdb.ListLeaseEventsRequest{TenantID: tenant, JobIDs: []string{entry.Key.JobId}, PageSize: 200, PageToken: page})
		if e != nil {
			return status{}, errFixture
		}
		for _, event := range events.Events {
			if event.EventType == jobdb.LeaseEventAcquired {
				s.Acquisitions++
			}
			if event.EventType == jobdb.LeaseEventRenewed && event.LeaseID == entry.LeaseID && event.PreviousLeaseExpiresAt != nil && event.LeaseExpiresAt.After(*event.PreviousLeaseExpiresAt) {
				s.Renewals++
				if event.LeaseExpiresAt.After(s.LatestExpiry) {
					s.LatestExpiry = event.LeaseExpiresAt
				}
				if event.OccurredAt.After(entry.InitialExpiry) {
					s.RenewedBeyondInitial = true
				}
			}
		}
		if events.NextPageToken == "" {
			break
		}
		page = events.NextPageToken
	}
	return s, nil
}

func (f *fixture) serveControl(path string) error {
	parent, err := os.Lstat(filepath.Dir(path))
	if !filepath.IsAbs(path) || len(path) > 100 || err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return errFixture
	}
	owner, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return errFixture
	}
	f.controlListener, err = net.Listen("unix", path)
	if err != nil {
		return errFixture
	}
	if os.Chmod(path, 0600) != nil {
		f.controlListener.Close()
		return errFixture
	}
	type authenticated struct{}
	f.control = &http.Server{ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, ErrorLog: log.New(io.Discard, "", 0), ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		u, ok := c.(*net.UnixConn)
		if !ok {
			return ctx
		}
		raw, e := u.SyscallConn()
		if e != nil {
			return ctx
		}
		good := false
		if raw.Control(func(fd uintptr) {
			cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
			good = e == nil && cred.Uid == uint32(os.Geteuid())
		}) != nil {
			return ctx
		}
		return context.WithValue(ctx, authenticated{}, good)
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Context().Value(authenticated{}) != true || r.Header.Get("X-Pulse-Fixture") != f.identity {
			http.Error(w, "private fixture required", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "unsupported fixture operation", http.StatusMethodNotAllowed)
			return
		}
		var result any
		var err error
		switch r.URL.Path {
		case "/health":
			result = map[string]bool{"ready": true}
		case "/launch":
			query := r.URL.Query()
			profile := query.Get("profile")
			if profile == "" {
				profile = "renewal"
			}
			result, err = f.launchProfile(r.Context(), query.Get("name"), query.Get("case"), profile)
		case "/status":
			result, err = f.status(r.Context(), r.URL.Query().Get("name"))
		default:
			err = errFixture
		}
		if err != nil {
			http.Error(w, "fixture operation unavailable", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})}
	go f.control.Serve(f.controlListener)
	return nil
}

func (f *fixture) Close() {
	f.once.Do(func() {
		if f.control != nil {
			f.control.Close()
		}
		if f.controlListener != nil {
			f.controlListener.Close()
		}
		if f.guest != nil {
			f.guest.Close()
		}
		if f.guestListener != nil {
			f.guestListener.Close()
		}
		f.mu.Lock()
		clear(f.cases)
		f.mu.Unlock()
		if f.db != nil {
			f.db.Close(context.Background())
		}
		if f.dir != "" {
			os.RemoveAll(f.dir)
		}
	})
}

func main() {
	control := flag.String("control-socket", "", "private Unix socket path")
	listen := flag.String("listen", "127.0.0.1:18880", "guest JobDB/Git listen address")
	endpoint := flag.String("endpoint", "http://127.0.0.1:18880", "operator-provided guest-reachable URL")
	leaseDuration := flag.Duration("lease-duration", time.Minute, "initial lease duration (1s to 10m); workload lasts longer than this")
	identity := flag.String("identity", "cloudhypervisor-fixture", "fixture descriptor identity")
	dockerImage := flag.String("docker-image", "", "locally preloaded Docker image ID for the docker profile")
	guestAddresses := flag.String("guest-addresses", "", "comma-separated guest IPv4 addresses; required on a non-loopback listener")
	flag.Parse()
	var peers []netip.Addr
	if *guestAddresses != "" {
		for _, value := range strings.Split(*guestAddresses, ",") {
			addr, err := netip.ParseAddr(value)
			if err != nil || !addr.Is4() || !addr.IsPrivate() {
				fmt.Fprintln(os.Stderr, "invalid fixture guest network")
				os.Exit(1)
			}
			peers = append(peers, addr)
		}
	}
	addr, err := netip.ParseAddrPort(*listen)
	if err != nil || !addr.Addr().IsLoopback() && (len(peers) == 0 || !addr.Addr().IsPrivate()) {
		fmt.Fprintln(os.Stderr, "private guest network binding required")
		os.Exit(1)
	}
	if *dockerImage != "" && !dockerImageID.MatchString(*dockerImage) {
		fmt.Fprintln(os.Stderr, "FAIL: Docker profile requires a fixed image ID")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 30*time.Minute)
	defer timeout()
	f, err := newFixture(ctx, *listen, *endpoint, *identity, *leaseDuration, peers...)
	if err == nil {
		defer f.Close()
		f.dockerImage = *dockerImage
		err = f.serveControl(*control)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		fmt.Fprintln(os.Stderr, "FAIL: private guest fixture unavailable")
		os.Exit(1)
	}
	fmt.Println("private guest fixture ready")
	<-ctx.Done()
}

package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

var bg = context.Background()

const syntheticLease = "SYNTHETIC-PRIVATE-LEASE-\x00-unchanged\n"

type fakeNative struct {
	mu               sync.Mutex
	starts           []runner.Spec
	leases           []api.Lease
	uncertain        bool
	observation      observation
	observeError     error
	entered, release chan struct{}
}

func (f *fakeNative) preflight(context.Context, int) error { return nil }
func (f *fakeNative) start(ctx context.Context, _ string, s runner.Spec, l api.Lease, sub *subscription) (bool, error) {
	f.mu.Lock()
	f.starts = append(f.starts, s)
	f.leases = append(f.leases, l)
	uncertain := f.uncertain
	f.mu.Unlock()
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	if sub != nil {
		for _, src := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
			sub.source(s.LaunchID, src, api.SourceActive)
		}
	}
	if uncertain || ctx.Err() != nil {
		return false, ErrObservation
	}
	return true, nil
}
func (f *fakeNative) observe(context.Context, string, runner.Spec) (observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observation, f.observeError
}
func (f *fakeNative) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) }
func shortDir(t *testing.T) string {
	t.Helper()
	d, e := os.MkdirTemp("/tmp", "pc-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
func testConfig(t *testing.T) Config {
	t.Helper()
	digest := strings.Repeat("a", 64)
	a := Asset{Path: "/fixture/asset", SHA256: digest}
	return Config{Version: 1, Owner: "test", StateDir: shortDir(t), HostCPUMillis: 4000, HostMemoryBytes: 8 << 30, HostDiskBytes: 32 << 30, Network: []Network{{TAP: "test0", Address: "192.0.2.2", Gateway: "192.0.2.1", Netmask: "255.255.255.0", DNS: "192.0.2.1"}}, Bases: []Base{{Name: "ubuntu-24.04-arm64", Architecture: "arm64", Disk: a, Kernel: a, Recipe: digest, Preparation: a, DockerFixtureDigest: "sha256:" + digest, Tools: map[string]string{"c2j": digest, "nix": digest, "docker": digest, "guest-agent": digest}, Capabilities: []string{"docker"}, UsableCPUMillis: 4000, UsableMemoryBytes: 7 << 30}}}
}
func testRuntime(t *testing.T) (*Runtime, *fakeNative) {
	t.Helper()
	p, e := New(testConfig(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	f := &fakeNative{observation: observation{state: "running"}}
	p.backend = f
	p.ready = func(Base) bool { return true }
	p.qualified = p.ready
	return p, f
}
func launch(id string) api.Launch {
	return api.Launch{LaunchID: id, Allocation: api.Allocation{Base: "ubuntu*", CPUMillis: 1000, MemoryBytes: 1 << 30, Capabilities: []string{"docker"}}, Job: api.JobRef{JobDB: "https://jobdb.invalid/tenant", InstanceID: "instance", TenantID: "tenant", JobID: "job"}, Lease: api.Lease(syntheticLease)}
}
func submit(t *testing.T, p *Runtime, l api.Launch) api.Submission {
	t.Helper()
	rs, s, e := p.Submit(bg, []api.Launch{l}, api.SubmitOptions{})
	if e != nil || s != nil || len(rs) != 1 {
		t.Fatalf("submit failed: %v", e)
	}
	return rs[0]
}
func TestProviderContract(t *testing.T) {
	var _ api.Provider = (*Runtime)(nil)
	if api.MaxBatch != 100 {
		t.Fatal("contract changed")
	}
}
func TestConfigCopiesAndLocks(t *testing.T) {
	c := testConfig(t)
	p, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if _, e = New(c); e == nil {
		t.Fatal("second owner lock acquired")
	}
	c.Bases[0].Capabilities[0] = "foreign"
	c.Bases[0].Tools["nix"] = "mutated"
	c.Network[0].TAP = "mutated"
	if p.config.Bases[0].Capabilities[0] != "docker" || p.config.Bases[0].Tools["nix"] == "mutated" || p.config.Network[0].TAP == "mutated" {
		t.Fatal("configuration aliases caller")
	}
	if len(p.records) != 0 {
		t.Fatal("constructor launched work")
	}
	c2 := testConfig(t)
	q, e := New(c2)
	if e != nil {
		t.Fatal(e)
	}
	q.Close()
}
func TestHomeDefaultsAndIsolation(t *testing.T) {
	h := shortDir(t)
	t.Setenv("HOME", h)
	c := Config{Version: 1, Owner: "a"}
	p, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if p.config.StateDir != filepath.Join(h, ".pulse/cloudhypervisor/owners/a") {
		t.Fatal("wrong owner default")
	}
	path, e := DefaultConfigPath()
	if e != nil || path != filepath.Join(h, ".pulse/cloudhypervisor/config.json") {
		t.Fatal("wrong command default")
	}
	c.Owner = "b"
	q, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	q.Close()
	if len(q.records) != 0 {
		t.Fatal("cross-owner recovery")
	}
	t.Setenv("HOME", shortDir(t))
	r, e := New(Config{Version: 1, Owner: "a"})
	if e != nil {
		t.Fatal(e)
	}
	r.Close()
	if r.config.StateDir == p.config.StateDir {
		t.Fatal("home change migrated state")
	}
}
func TestConfigUnsafeAndExplicitPaths(t *testing.T) {
	for _, h := range []string{"", "relative", filepath.Join(shortDir(t), "missing-home")} {
		t.Setenv("HOME", h)
		if _, e := Normalize(Config{Version: 1, Owner: "test"}); e == nil {
			t.Fatal("unsafe home accepted")
		}
		if _, e := DefaultConfigPath(); e == nil {
			t.Fatal("unsafe settings home accepted")
		}
	}
	c := testConfig(t)
	if _, e := Normalize(c); e != nil {
		t.Fatal("explicit paths required home")
	}
	for _, owner := range []string{"../x", ".", "..", "a/b", "a\n"} {
		bad := c
		bad.Owner = owner
		if _, e := New(bad); e == nil {
			t.Fatal("unsafe owner accepted")
		}
	}
	for _, dir := range []string{"relative", c.StateDir + "/../escape", "/tmp/" + strings.Repeat("x", 100)} {
		bad := c
		bad.StateDir = dir
		if _, e := New(bad); e == nil {
			t.Fatal("unsafe path accepted")
		}
	}
	bad := c
	bad.Version = 2
	if _, e := New(bad); e == nil {
		t.Fatal("unknown version accepted")
	}
	symlink := filepath.Join(shortDir(t), "link")
	if os.Symlink(c.StateDir, symlink) != nil {
		t.Fatal("symlink fixture")
	}
	bad = c
	bad.StateDir = symlink
	if _, e := New(bad); e == nil {
		t.Fatal("symlink accepted")
	}
	canary := filepath.Join(shortDir(t), "unrelated")
	os.WriteFile(canary, []byte("keep"), 0644)
	if b, _ := os.ReadFile(canary); string(b) != "keep" {
		t.Fatal("unrelated data changed")
	}
}
func TestCapabilitiesIndependentAndReadOnly(t *testing.T) {
	p, f := testRuntime(t)
	first, e := p.GetCapabilities(bg)
	if e != nil || len(first.Bases) != 1 {
		t.Fatal(e)
	}
	first.Bases[0].Capabilities[0] = "bad"
	first.Bases[0].Name = "bad"
	if r := submit(t, p, launch("busy")); r.Status != api.Accepted || r.Reason != "" {
		t.Fatal(r)
	}
	second, e := p.GetCapabilities(bg)
	if e != nil || second.Bases[0].Name == "bad" || second.Bases[0].Capabilities[0] != "docker" || f.count() != 1 {
		t.Fatal("discovery changed native state")
	}
}
func TestWholeBatchValidation(t *testing.T) {
	for _, kind := range []string{"empty", "oversized", "duplicate", "id", "endpoint", "lease", "job", "quantity", "negative", "timeout", "negative-timeout"} {
		t.Run(kind, func(t *testing.T) {
			p, f := testRuntime(t)
			ls := []api.Launch{launch("valid"), launch("second")}
			switch kind {
			case "empty":
				ls = nil
			case "oversized":
				for i := 2; i <= 100; i++ {
					ls = append(ls, launch(fmt.Sprint(i)))
				}
			case "duplicate":
				ls[1].LaunchID = ls[0].LaunchID
			case "id":
				ls[1].LaunchID = "bad/id"
			case "endpoint":
				ls[1].Job.JobDB = "https://user:password@job.invalid/tenant"
			case "lease":
				ls[1].Lease = ""
			case "job":
				ls[1].Job.InstanceID = ""
			case "quantity":
				ls[1].MemoryBytes = api.MaxQuantity + 1
			case "negative":
				ls[1].CPUMillis = -1
			case "timeout":
				ls[1].TimeoutSeconds = 31536001
			case "negative-timeout":
				ls[1].TimeoutSeconds = -1
			}
			rs, _, e := p.Submit(bg, ls, api.SubmitOptions{})
			if e == nil || len(rs) != 0 || f.count() != 0 || len(p.records) != 0 {
				t.Fatal("structurally invalid batch had side effects")
			}
		})
	}
}
func TestLiteralMatchingAndCapabilities(t *testing.T) {
	for _, c := range []struct {
		p, v string
		want bool
	}{{"ubuntu*", "ubuntu-24.04-arm64", true}, {"*", "a/b", true}, {"u**a*", "ubuntu/a", true}, {"ubuntu[0-9]", "ubuntu2", false}, {"Ubuntu*", "ubuntu", false}, {"ubuntu", "ubuntu-24", false}, {"ab*bc", "abc", false}, {"a*b*c", "abc", true}} {
		if Match(c.p, c.v) != c.want {
			t.Fatalf("match %q", c.p)
		}
	}
	p, f := testRuntime(t)
	l := launch("unsupported")
	l.Capabilities = []string{"Docker"}
	if r := submit(t, p, l); r.Status != api.Unsupported {
		t.Fatal(r)
	}
	if f.count() != 0 || l.Base != "ubuntu*" {
		t.Fatal("constraints broadened")
	}
	if !allOf([]string{"docker", "docker"}, []string{"docker"}) {
		t.Fatal("duplicate capability semantics")
	}
}
func TestUnsupportedConstraintsAndMixedResults(t *testing.T) {
	p, f := testRuntime(t)
	a, b, c := launch("scratch"), launch("deadline"), launch("accepted")
	a.ScratchBytes = 1
	now := time.Now()
	b.StartBefore = &now
	rs, _, e := p.Submit(bg, []api.Launch{a, b, c}, api.SubmitOptions{})
	if e != nil || len(rs) != 3 || f.count() != 1 {
		t.Fatal(e)
	}
	for _, r := range rs {
		if r.LaunchID == "accepted" {
			if r.Status != api.Accepted || r.Reason != "" {
				t.Fatal(r)
			}
		} else if r.Status != api.Unsupported || r.Reason == "" {
			t.Fatal(r)
		}
	}
	f.mu.Lock()
	s := f.starts[0]
	lease := f.leases[0]
	f.mu.Unlock()
	if s.Facts.Base == c.Base || s.Facts.MemoryBytes != 7<<30 || s.Facts.CPUMillis != 4000 || s.Job != c.Job || lease != c.Lease {
		t.Fatal("facts or supplied lease changed")
	}
	assertNoCredentialFiles(t, p.config.StateDir)
}
func assertNoCredentialFiles(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), syntheticLease) || strings.Contains(string(b), "SYNTHETIC-PRIVATE-LEASE") {
				t.Error("credential persisted")
			}
		}
		return nil
	})
}
func TestConcurrentCapacity(t *testing.T) {
	p, f := testRuntime(t)
	var wg sync.WaitGroup
	results := make(chan api.Status, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rs, _, e := p.Submit(bg, []api.Launch{launch(fmt.Sprint(i))}, api.SubmitOptions{})
			if e != nil {
				t.Error(e)
				return
			}
			results <- rs[0].Status
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for s := range results {
		if s == api.Accepted {
			accepted++
		} else if s != api.NoCapacity {
			t.Error(s)
		}
	}
	if accepted != 1 || f.count() != 1 {
		t.Fatal("oversubscribed host")
	}
	zero := 0
	p.config.Count = &zero
	if submit(t, p, launch("drained")).Status != api.NoCapacity || len(p.records) != 1 {
		t.Fatal("drain canceled reservation")
	}
}
func TestWarmingZeroAndBudgetDecline(t *testing.T) {
	for _, kind := range []string{"warming", "zero", "cpu", "memory", "disk"} {
		t.Run(kind, func(t *testing.T) {
			p, f := testRuntime(t)
			switch kind {
			case "warming":
				p.ready = func(Base) bool { return false }
			case "zero":
				v := 0
				p.config.Count = &v
			case "cpu":
				p.config.HostCPUMillis = 3999
			case "memory":
				p.config.HostMemoryBytes = (8 << 30) - 1
			case "disk":
				p.config.HostDiskBytes = (32 << 30) - 1
			}
			if submit(t, p, launch("decline")).Status != api.NoCapacity || f.count() != 0 || len(p.records) != 0 {
				t.Fatal("decline retained work")
			}
		})
	}
}
func TestCancellationBoundaries(t *testing.T) {
	p, f := testRuntime(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	rs, _, e := p.Submit(ctx, []api.Launch{launch("before")}, api.SubmitOptions{})
	if e == nil || rs[0].Status != api.Unavailable || f.count() != 0 {
		t.Fatal("pre-dispatch cancellation")
	}
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	ctx, cancel = context.WithCancel(bg)
	done := make(chan []api.Submission, 1)
	go func() { rs, _, _ := p.Submit(ctx, []api.Launch{launch("after")}, api.SubmitOptions{}); done <- rs }()
	<-f.entered
	cancel()
	close(f.release)
	rs = <-done
	if rs[0].Status != api.Unknown || len(p.records) != 1 || f.count() != 1 {
		t.Fatal("cancellation lost reservation")
	}
}
func TestRecoveryNeverReplaysAndFencesCompletion(t *testing.T) {
	p, f := testRuntime(t)
	c := p.config
	f.uncertain = true
	rs, _, e := p.Submit(bg, []api.Launch{launch("uncertain")}, api.SubmitOptions{})
	if e == nil || rs[0].Status != api.Unknown {
		t.Fatal("lost acknowledgement became definite")
	}
	p.Close()
	q, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	q.backend = f
	q.ready = func(Base) bool { return true }
	if submit(t, q, launch("uncertain")).Status != api.Rejected || f.count() != 1 {
		t.Fatal("replayed on reopen")
	}
	if e = q.Reconcile(bg); e != nil || len(q.records) != 1 {
		t.Fatal("live runner lost")
	}
	f.mu.Lock()
	f.observation = observation{terminated: true}
	f.mu.Unlock()
	if e = q.Reconcile(bg); e != nil || len(q.records) != 0 {
		t.Fatal(e)
	}
	q.Close()
	r, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	r.backend = f
	r.ready = func(Base) bool { return true }
	if submit(t, r, launch("uncertain")).Status != api.Rejected || f.count() != 1 {
		t.Fatal("terminal identity forgotten")
	}
}
func TestIntentFailurePreventsDispatch(t *testing.T) {
	for _, point := range []string{"write", "file-sync", "rename", "directory-sync"} {
		t.Run(point, func(t *testing.T) {
			p, f := testRuntime(t)
			p.writeHook = func(s string) error {
				if s == point {
					return errors.New("injected")
				}
				return nil
			}
			rs, _, e := p.Submit(bg, []api.Launch{launch("fault")}, api.SubmitOptions{})
			if e == nil || rs[0].Status != api.Unavailable || f.count() != 0 || len(p.records) != 1 {
				t.Fatal("failed intent dispatched or freed uncertain write")
			}
			assertNoCredentialFiles(t, p.config.StateDir)
			c := p.config
			p.Close()
			q, e := New(c)
			if e == nil {
				defer q.Close()
				if len(q.records) != 1 {
					t.Fatal("reservation missing after persisted rename")
				}
			} else if point == "rename" || point == "directory-sync" {
				t.Fatal(e)
			}
		})
	}
}
func TestListPaginationAndObservationErrors(t *testing.T) {
	p, f := testRuntime(t)
	one := launch("one")
	if submit(t, p, one).Status != api.Accepted {
		t.Fatal("admission")
	}
	// Add a second durable fixture without broadening real admission capacity.
	s := p.records["one"]
	s.LaunchID = "two"
	s.Identity = identity()
	p.records["two"] = s
	first, e := p.List(bg, api.ListRequest{PageSize: 1})
	if e != nil || len(first.Items) != 1 || first.NextPageToken == "" || first.Items[0].Job != one.Job {
		t.Fatal("first page")
	}
	next, e := p.List(bg, api.ListRequest{PageSize: 1, PageToken: first.NextPageToken})
	if e != nil || len(next.Items) != 1 || next.Items[0].LaunchID == first.Items[0].LaunchID {
		t.Fatal("continuation")
	}
	for _, r := range []api.ListRequest{{PageToken: "bad"}, {PageSize: 101}, {PageToken: first.NextPageToken, LaunchID: "one"}} {
		if _, e = p.List(bg, r); e == nil {
			t.Fatal("invalid page accepted")
		}
	}
	other, _ := testRuntime(t)
	if _, e = other.List(bg, api.ListRequest{PageToken: first.NextPageToken}); e == nil {
		t.Fatal("foreign owner token")
	}
	first.Items[0].Refs[0] = "mutated"
	page, _ := p.List(bg, api.ListRequest{LaunchID: "one"})
	if page.Items[0].Refs[0] == "mutated" {
		t.Fatal("listing aliases internal state")
	}
	f.mu.Lock()
	f.observeError = ErrObservation
	f.mu.Unlock()
	if _, e = p.List(bg, api.ListRequest{}); e == nil {
		t.Fatal("inventory failure became empty success")
	}
	if f.count() != 1 {
		t.Fatal("listing mutated native work")
	}
}
func TestCleanupRequiresOwnershipAndTermination(t *testing.T) {
	p, f := testRuntime(t)
	submit(t, p, launch("owned"))
	if e := p.Reconcile(bg); e != nil || len(p.records) != 1 {
		t.Fatal("age or absence freed work")
	}
	f.mu.Lock()
	f.observation = observation{terminated: true}
	f.mu.Unlock()
	canary := filepath.Join(p.dir("owned"), "foreign")
	os.WriteFile(canary, []byte("keep"), 0600)
	if e := p.Reconcile(bg); e == nil || len(p.records) != 1 {
		t.Fatal("foreign resource failed to quarantine")
	}
	if b, _ := os.ReadFile(canary); string(b) != "keep" {
		t.Fatal("foreign canary deleted")
	}
	os.Remove(canary)
	target := filepath.Join(shortDir(t), "canary")
	os.WriteFile(target, []byte("keep"), 0600)
	link := filepath.Join(p.dir("owned"), "disk.raw")
	os.Symlink(target, link)
	if e := p.Reconcile(bg); e == nil || len(p.records) != 1 {
		t.Fatal("symlink failed to quarantine")
	}
	os.Remove(link)
	apiLock := filepath.Join(p.dir("owned"), "api.lock")
	if err := os.WriteFile(apiLock, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reconcile(bg); err == nil || len(p.records) != 1 {
		t.Fatal("nonempty API lock was treated as stale")
	}
	if err := os.Truncate(apiLock, 0); err != nil {
		t.Fatal(err)
	}
	held, err := lockAPISidecar(p.dir("owned"))
	if err != nil || held == nil {
		t.Fatal("take the VMM's API lock")
	}
	if err := p.Reconcile(bg); err == nil || len(p.records) != 1 {
		held.Close()
		t.Fatal("held VMM API lock did not quarantine resources")
	}
	held.Close()
	if e := p.Reconcile(bg); e != nil || len(p.records) != 0 {
		t.Fatal(e)
	}
	if _, err := os.Lstat(apiLock); !os.IsNotExist(err) {
		t.Fatal("stale VMM API lock survived cleanup")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("foreign target modified")
	}
}
func TestCloseDoesNotCancelAcceptedWork(t *testing.T) {
	p, f := testRuntime(t)
	submit(t, p, launch("accepted"))
	if p.Close() != nil {
		t.Fatal("close")
	}
	if f.count() != 1 {
		t.Fatal("close changed native starts")
	}
	rs, _, e := p.Submit(bg, []api.Launch{launch("closed")}, api.SubmitOptions{})
	if e == nil || rs[0].Status != api.Unavailable {
		t.Fatal("closed provider admitted")
	}
	if len(p.records) != 1 {
		t.Fatal("close released reservation")
	}
}
func TestCorruptionQuarantines(t *testing.T) {
	p, _ := testRuntime(t)
	submit(t, p, launch("corrupt"))
	c := p.config
	p.Close()
	os.WriteFile(filepath.Join(p.dir("corrupt"), "record.json"), []byte(`{"version":999}`), 0600)
	if q, e := New(c); e == nil {
		q.Close()
		t.Fatal("corrupt store admitted new work")
	}
}
func TestConfigStrictAtomicSettings(t *testing.T) {
	c := testConfig(t)
	path := filepath.Join(shortDir(t), "config.json")
	if SaveConfig(path, c) != nil {
		t.Fatal("save")
	}
	got, e := LoadConfig(path)
	if e != nil || got.StateDir != c.StateDir {
		t.Fatal(e)
	}
	os.WriteFile(path, []byte(`{"version":1,"owner":"test","unknown":"private"}`), 0600)
	if _, e = LoadConfig(path); e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal("unknown field or unsafe error")
	}
	os.WriteFile(path, []byte(`{"version":1,"owner":"test","state_dir":"state"}`), 0600)
	got, e = LoadConfig(path)
	if e != nil || got.StateDir != filepath.Join(filepath.Dir(path), "state") {
		t.Fatal("relative settings resolution")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("settings exposed")
	}
	data, _ := json.Marshal(c)
	if _, e = DecodeConfig(append(data, []byte(` {}`)...)); e == nil {
		t.Fatal("trailing config accepted")
	}
}
func TestUpgradeTimeoutPreservesAcceptedWork(t *testing.T) {
	p, f := testRuntime(t)
	submit(t, p, launch("running"))
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if e := p.Upgrade(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if len(p.records) != 1 || f.count() != 1 || p.draining {
		t.Fatal("maintenance canceled work or left drain")
	}
}
func TestStoreFilesPrivate(t *testing.T) {
	p, _ := testRuntime(t)
	submit(t, p, launch("private"))
	filepath.WalkDir(p.config.StateDir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			t.Error(e)
			return nil
		}
		info, _ := d.Info()
		if d.IsDir() {
			if info.Mode().Perm() != 0700 {
				t.Error("directory exposed")
			}
		} else if info.Mode().IsRegular() && info.Mode().Perm() != 0600 {
			t.Error("file exposed")
		}
		return nil
	})
	assertNoCredentialFiles(t, p.config.StateDir)
}

func TestBuilderReservationsSurviveReopenAndStayOutOfList(t *testing.T) {
	p, f := testRuntime(t)
	c := p.config
	root := filepath.Join(c.StateDir, "sources")
	if state.PrivateDir(root) != nil {
		t.Fatal("source root")
	}
	dir := filepath.Join(root, "build-fixture")
	state.PrivateDir(dir)
	s := runner.Spec{Version: 1, Owner: c.Owner, LaunchID: "prepare", Identity: identity(), Slot: 0, VCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 32 << 30, Source: "/fixture/source"}
	if state.Atomic(filepath.Join(dir, "record.json"), s, nil) != nil {
		t.Fatal("builder intent")
	}
	p.Close()
	q, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	q.backend = f
	q.ready = func(Base) bool { return true }
	if len(q.builders) != 1 {
		t.Fatal("builder reservation forgotten")
	}
	if submit(t, q, launch("waiting")).Status != api.NoCapacity || f.count() != 0 {
		t.Fatal("builder budget oversubscribed")
	}
	list, e := q.List(bg, api.ListRequest{})
	if e != nil || len(list.Items) != 0 {
		t.Fatal("builder exposed as assignment")
	}
	if e = q.Reconcile(bg); e != nil || len(q.builders) != 1 {
		t.Fatal("live builder released")
	}
	f.mu.Lock()
	f.observation = observation{terminated: true}
	f.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "api.lock"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if e = q.Reconcile(bg); e != nil || len(q.builders) != 0 {
		t.Fatal(e)
	}
	if submit(t, q, launch("after-builder")).Status != api.Accepted {
		t.Fatal("terminated builder did not release capacity")
	}
}
func TestConcurrentNetworkIdentities(t *testing.T) {
	c := testConfig(t)
	count := 2
	c.Count = &count
	c.HostCPUMillis = 8000
	c.HostMemoryBytes = 16 << 30
	c.HostDiskBytes = 64 << 30
	c.Network = append(c.Network, Network{TAP: "test1", Address: "192.0.2.3", Gateway: "192.0.2.1", Netmask: "255.255.255.0", DNS: "192.0.2.1"})
	p, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	f := &fakeNative{observation: observation{state: "running"}}
	p.backend = f
	p.ready = func(Base) bool { return true }
	p.qualified = p.ready
	rs, _, e := p.Submit(bg, []api.Launch{launch("first"), launch("second")}, api.SubmitOptions{})
	if e != nil || rs[0].Status != api.Accepted || rs[1].Status != api.Accepted {
		t.Fatal("two slot fixture")
	}
	a, b := f.starts[0], f.starts[1]
	if a.MAC == b.MAC || a.Address == b.Address || a.TAP == b.TAP || a.CID == b.CID || a.Identity == b.Identity || a.Unit == b.Unit || p.dir(a.LaunchID) == p.dir(b.LaunchID) {
		t.Fatal("guest identities overlap")
	}
}
func TestMixedBatchKeepsExplicitResultsAlongsideError(t *testing.T) {
	p, f := testRuntime(t)
	f.uncertain = true
	l := launch("bad-ack")
	unsupported := launch("unsupported")
	unsupported.Base = "different"
	rs, _, e := p.Submit(bg, []api.Launch{unsupported, l}, api.SubmitOptions{})
	if e == nil || len(rs) != 2 || rs[0].Status != api.Unsupported || rs[1].Status != api.Unknown || rs[0].LaunchID == rs[1].LaunchID {
		t.Fatal("explicit outcomes lost beside method error")
	}
}

func TestDefaultRootNeverPersistsLeaseEvenOnStorageFailure(t *testing.T) {
	h := shortDir(t)
	t.Setenv("HOME", h)
	c := testConfig(t)
	c.StateDir = ""
	c.Owner = "d"
	p, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	f := &fakeNative{}
	p.backend = f
	p.ready = func(Base) bool { return true }
	p.qualified = p.ready
	p.writeHook = func(s string) error {
		if s == "write" {
			return errors.New("injected")
		}
		return nil
	}
	_, _, e = p.Submit(bg, []api.Launch{launch("secret")}, api.SubmitOptions{})
	if e == nil || f.count() != 0 {
		t.Fatal("intent fault missed")
	}
	assertNoCredentialFiles(t, h)
}
func TestRunnerPathsStayAbsoluteAfterHomeChanges(t *testing.T) {
	p, f := testRuntime(t)
	dir := p.config.StateDir
	submit(t, p, launch("path"))
	c := p.config
	p.Close()
	t.Setenv("HOME", shortDir(t))
	q, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	q.backend = f
	if q.config.StateDir != dir || !filepath.IsAbs(q.dir("path")) || len(q.records) != 1 || f.count() != 1 {
		t.Fatal("resolved owner path changed")
	}
}

func TestCloseRacingDispatchKeepsSingleAcceptedExecution(t *testing.T) {
	p, f := testRuntime(t)
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	submitted := make(chan []api.Submission, 1)
	go func() { rs, _, _ := p.Submit(bg, []api.Launch{launch("racing")}, api.SubmitOptions{}); submitted <- rs }()
	<-f.entered
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	close(f.release)
	rs := <-submitted
	<-closed
	if rs[0].Status != api.Accepted || f.count() != 1 || len(p.records) != 1 {
		t.Fatal("close race canceled or replayed native dispatch")
	}
}

func TestSharedPathResolverReadsHomeOnce(t *testing.T) {
	h := shortDir(t)
	calls := 0
	r := PathResolver{lookup: func() (string, error) { calls++; return h, nil }}
	path, e := r.ConfigPath("")
	if e != nil {
		t.Fatal(e)
	}
	c, e := r.Normalize(Config{Version: 1, Owner: "a"})
	if e != nil || calls != 1 || path != filepath.Join(h, ".pulse/cloudhypervisor/config.json") || c.StateDir != filepath.Join(h, ".pulse/cloudhypervisor/owners/a") {
		t.Fatal("settings and owner defaults used different home resolutions")
	}
	explicit := PathResolver{lookup: func() (string, error) { t.Fatal("explicit paths resolved HOME"); return "", nil }}
	if _, e = explicit.ConfigPath("/explicit/config.json"); e != nil {
		t.Fatal(e)
	}
	if _, e = explicit.Normalize(Config{Version: 1, Owner: "a", StateDir: filepath.Join(h, "explicit")}); e != nil {
		t.Fatal(e)
	}
}

// The latest human direction limits supported native execution to ARM64/KVM.
func TestConfigARM64Only(t *testing.T) {
	c := testConfig(t)
	if _, err := Normalize(c); err != nil {
		t.Fatal("ARM64 base rejected", err)
	}
	for _, arch := range []string{"amd64", "riscv64", ""} {
		c.Bases[0].Architecture = arch
		c.Bases[0].Name = "ubuntu-24.04-" + arch
		if _, err := Normalize(c); err == nil {
			t.Fatal("unsupported architecture accepted", arch)
		}
	}
}

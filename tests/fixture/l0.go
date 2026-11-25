//go:build l0

// Physical-host qualification. Start the operator's candidate over its SDK
// interface and deliver real, disposable leases directly to that process.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/colony-2/c2j/pkg/recipe"
	"github.com/colony-2/c2j/pkg/workflowctl"
	"github.com/colony-2/jobdb/pkg/jobdb"
	ch "github.com/colony-2/pulse-cloudhypervisor"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

type l0Case struct {
	name        string
	kvm, docker bool
}

func l0Cases(kvm, docker bool) []l0Case {
	cases := []l0Case{{name: "ordinary"}}
	if docker {
		cases = append(cases, l0Case{name: "docker", docker: true})
	}
	if kvm {
		cases = append(cases, l0Case{name: "kvm", kvm: true})
		if docker {
			cases = append(cases, l0Case{name: "docker-kvm", docker: true, kvm: true})
		}
	}
	return cases
}

type l0Pool struct {
	config                        ch.Config
	configPath, repo, egress, kit string
	base                          ch.Base
}

func command(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	return cmd.Run()
}

func (p l0Pool) runCase(ctx context.Context, f *fixture, test l0Case) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 16*time.Minute)
	defer cancel()
	name := fmt.Sprintf("l0-%s-%d", test.name, time.Now().UnixNano())
	file := "plain-job.yaml"
	job := workflowctl.StartJob{Inputs: map[string]any{"prompt": name}}
	allocation := api.Allocation{Base: p.base.Name}
	if test.docker {
		allocation.Capabilities = append(allocation.Capabilities, "docker")
		job.Inputs["docker_image"] = p.base.DockerFixtureDigest
	}
	if test.kvm {
		allocation.Capabilities = append(allocation.Capabilities, "kvm")
		allocation.CPUMillis, allocation.MemoryBytes = 2000, 2<<30
		file = "worker-job.yaml"
		job.Inputs["egress_url"] = p.egress
		a, err := jobdb.NewArtifactFromFileNoCleanup("worker.tar.gz", p.kit)
		if err != nil {
			return err
		}
		job.Artifacts = []jobdb.Artifact{a}
	}
	raw, err := os.ReadFile(filepath.Join(p.repo, "scripts/kvm", file))
	if err != nil {
		return err
	}
	rec, err := recipe.LoadRecipeFromString(raw)
	if err != nil {
		return err
	}

	data, err := json.Marshal(p.config)
	if err != nil {
		return err
	}
	client, err := plugin.Start(ctx, plugin.StartOptions{Executable: p.config.Runner, Args: []string{"server"},
		Env:  map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME"), "XDG_RUNTIME_DIR": os.Getenv("XDG_RUNTIME_DIR"), "DBUS_SESSION_BUS_ADDRESS": os.Getenv("DBUS_SESSION_BUS_ADDRESS")},
		Init: plugin.Initialization{Name: p.config.Owner, BaseDir: filepath.Dir(p.configPath), ConfigYAML: data}, CallTimeout: 2 * time.Minute})
	if err != nil {
		return fmt.Errorf("start candidate SDK server: %w", err)
	}
	// The SDK host leaves maintenance to its caller. Release terminated
	// reservations before the next case uses this same pool and TAP.
	defer func() {
		closeErr := client.Close()
		if closeErr != nil {
			err = errors.Join(err, closeErr)
			return
		}
		owner, openErr := ch.New(p.config)
		if openErr != nil {
			err = errors.Join(err, openErr)
			return
		}
		defer owner.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		err = errors.Join(err, owner.Reconcile(cleanup))
	}()
	a, err := f.launchRecipe(ctx, name, "valid", test.name, workload{}, *rec, job)
	if err != nil {
		return fmt.Errorf("create private assignment: %w", err)
	}
	ref := api.JobRef{JobDB: a.Job.JobDB, InstanceID: a.Job.InstanceID, TenantID: a.Job.TenantID, JobID: a.Job.JobID}
	fmt.Printf("RUN %s job=%s launch=%s\n", test.name, ref.JobID, name)
	launch := api.Launch{LaunchID: name, Allocation: allocation, Job: ref, Lease: api.Lease(a.Lease)}
	opts := api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx, OnLog: func(_ context.Context, log api.LaunchLog) error {
		if log.LaunchID == name && log.Source == api.LogSourceLaunch {
			fmt.Printf("%s launch=%s %s\n", log.Timestamp.UTC().Format(time.RFC3339Nano), name, log.Data)
		}
		return nil
	}}}
	out, sub, err := client.Submit(ctx, []api.Launch{launch}, opts)
	if sub != nil {
		defer sub.Stop()
	}
	if err != nil {
		return fmt.Errorf("submit assignment: %w", err)
	}
	if len(out) != 1 || out[0].LaunchID != name || out[0].Status != api.Accepted {
		return fmt.Errorf("admission: %+v", out)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		page, err := client.List(ctx, api.ListRequest{LaunchID: name})
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			break
		}
		if len(page.Items) != 1 || page.Items[0].Job != ref {
			return errors.New("candidate inventory does not match the assigned job")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	obs, err := f.l0Observation(ctx, name)
	if err != nil {
		return err
	}
	fmt.Println(obs)
	vm, err := checkObservation(test, name, obs)
	if err != nil {
		return err
	}
	if err = command(ctx, p.repo, "python3", "scripts/kvm/qualify.py", "match", p.config.StateDir, name, vm); err != nil {
		return err
	}
	return nil
}

func (f *fixture) l0Observation(ctx context.Context, name string) (string, error) {
	s, err := f.status(ctx, name)
	if err != nil {
		return "", err
	}
	chapters, err := f.runtime.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: f.cases[name].Key})
	if err != nil {
		return "", err
	}
	if s.State != string(jobdb.JobStatusCompleted) || s.Outcome != "success" || s.Acquisitions != 1 || s.ClaimAttempts != 0 {
		return "", fmt.Errorf("job ended with state=%s outcome=%s acquisitions=%d replacement_claims=%d%s", s.State, s.Outcome, s.Acquisitions, s.ClaimAttempts, f.failureDetails(ctx, f.cases[name].Key, chapters))
	}
	for _, c := range chapters {
		final, ok := c.Body.(jobdb.JobAttemptOutcomeChapter)
		if !ok {
			continue
		}
		output, ok := final.Outcome.(jobdb.ApplicationOutputOutcome)
		if !ok {
			continue
		}
		var r struct {
			Observation string
			Diagnostic  string
			ExitCode    *int `json:"exit_code"`
		}
		if err := json.Unmarshal(output.Output.Data, &r); err != nil {
			return "", err
		}
		if r.ExitCode == nil {
			return "", errors.New("missing probe exit code")
		}
		if *r.ExitCode != 0 {
			return "", fmt.Errorf("probe exit=%d\n%.4096s\n%.4096s", *r.ExitCode, r.Observation, r.Diagnostic)
		}
		if r.Observation == "" {
			return "", errors.New("missing probe observation")
		}
		return r.Observation, nil
	}
	return "", errors.New("completed job has no qualification output")
}

// Include the fixed probe's retained output before the temporary JobDB closes.
// Inputs, leases and the surrounding execution records stay in the fixture.
func (f *fixture) failureDetails(ctx context.Context, job jobdb.JobKey, chapters []jobdb.Chapter) string {
	var details strings.Builder
	for _, chapter := range chapters {
		if final, ok := chapter.Body.(jobdb.JobAttemptOutcomeChapter); ok {
			switch failure := final.Outcome.(type) {
			case jobdb.AppErrorOutcome:
				fmt.Fprintf(&details, "\nJob error: %.4096s", failure.Error.Message)
			case jobdb.SystemErrorOutcome:
				fmt.Fprintf(&details, "\nSystem error: %.4096s", failure.Error.Message)
			}
		}
		for _, artifact := range chapter.Artifacts {
			if artifact.Name != "_c2j/execution/probe.log" {
				continue
			}
			ref := jobdb.ArtifactRef{JobKey: job, Ordinal: chapter.Ordinal, Name: artifact.Name, Digest: artifact.Digest}
			retained, err := f.db.OpenArtifact(ctx, ref)
			if err != nil {
				fmt.Fprint(&details, "\nRetained probe log unavailable")
				continue
			}
			reader, err := retained.Open()
			if err != nil {
				fmt.Fprint(&details, "\nCould not open retained probe log")
				continue
			}
			const limit = 32 << 10
			fmt.Fprint(&details, "\nRetained probe log:\n")
			if retained.Size() > limit {
				_, err = io.CopyN(io.Discard, reader, retained.Size()-limit)
			}
			if err == nil {
				_, err = io.Copy(&details, io.LimitReader(reader, limit))
			}
			reader.Close()
			if err != nil {
				fmt.Fprint(&details, "\nCould not read the complete probe log")
			}
		}
	}
	return details.String()
}

var ordinaryIdentity = regexp.MustCompile(`^PASS ordinary worker launch=([^ ]+) identity=([a-f0-9]+) capabilities=\[.*\]$`)
var nestedIdentity = regexp.MustCompile(`^L1 launch=([^ ]+) identity=([a-f0-9]+) boot=[^ ]+ uid=1000 KVM API=12 VM created$`)

func checkObservation(test l0Case, launch, obs string) (string, error) {
	lines := strings.Split(strings.TrimSpace(obs), "\n")
	pattern := ordinaryIdentity
	var required []string
	if test.kvm {
		pattern = nestedIdentity
		for i := 1; i <= 3; i++ {
			required = append(required, fmt.Sprintf("PASS L2 virtio disk boot %d", i))
		}
		required = append(required, "PASS assigned worker KVM and three L2 disk boots", "PASS user-service peer, dynamic TAPs, peer and worker HTTP, DNS and outbound HTTP")
	}
	if test.docker {
		if test.kvm {
			required = append(required, "PASS Docker with KVM in user service")
		} else {
			required = append(required, "PASS Docker in ordinary worker")
		}
	}
	var vm string
	for _, line := range lines {
		if m := pattern.FindStringSubmatch(line); m != nil {
			if m[1] != launch || vm != "" {
				return "", errors.New("unexpected or duplicate guest launch identity")
			}
			vm = m[2]
		}
	}
	if vm == "" {
		return "", errors.New("missing guest launch identity")
	}
	for _, marker := range required {
		if !slices.Contains(lines, marker) {
			return "", fmt.Errorf("missing %q", marker)
		}
	}
	return vm, nil
}

func qualify(ctx context.Context, repo, configPath, expect, egress, cache string) error {
	if expect != "supported" && expect != "unsupported" {
		return errors.New("--expect-kvm must be supported or unsupported")
	}
	nested := expect == "supported"
	if nested {
		u, err := url.Parse(egress)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil || u.User != nil || u.Fragment != "" || strings.ContainsAny(egress, " \t\r\n") {
			return errors.New("--egress-url must be an HTTP URL with a DNS hostname, reachable from a nested guest")
		}
	}
	c, err := ch.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load dedicated pool %s: %w", configPath, err)
	}
	c, err = ch.Normalize(c)
	if err != nil {
		return err
	}
	var base ch.Base
	for _, b := range c.Bases {
		if b.Architecture == runtime.GOARCH {
			base = b
			break
		}
	}
	if base.Name == "" || len(c.Network) == 0 {
		return errors.New("configure a native Ubuntu base and an available network slot")
	}
	peers := make([]netip.Addr, 0, len(c.Network))
	gateway, err := netip.ParseAddr(c.Network[0].Gateway)
	if err != nil || !gateway.IsPrivate() {
		return errors.New("qualification needs a host-local private IPv4 gateway")
	}
	for _, slot := range c.Network {
		peer, err := netip.ParseAddr(slot.Address)
		if err != nil || !peer.IsPrivate() {
			return errors.New("qualification needs private IPv4 worker addresses")
		}
		peers = append(peers, peer)
	}
	binary, err := os.Open(c.Runner)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, binary)
	binary.Close()
	if err != nil {
		return err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return err
	}
	fmt.Printf("L0 linux/%s kernel=%s base=%s fingerprint=%s guest-agent=%s runner=%s sha256=%x expected-kvm=%s\n",
		runtime.GOARCH, strings.TrimSpace(string(kernel)), base.Name, ch.Fingerprint(base), base.Tools["guest-agent"], c.Runner, h.Sum(nil), expect)
	fmt.Println("Running candidate setup")
	if err := command(ctx, repo, c.Runner, "setup", "--config", configPath); err != nil {
		return fmt.Errorf("candidate setup: %w", err)
	}
	if err := command(ctx, repo, "go", "run", "./scripts/kvm/testdata/catalog.go", "--config", configPath, "--provider", c.Runner, "--expect", expect); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	var prepared struct{ Fingerprint, Digest string }
	proof, err := os.ReadFile(filepath.Join(c.StateDir, "sources", ch.Fingerprint(base)+".json"))
	if err != nil {
		return fmt.Errorf("read prepared source proof: %w", err)
	}
	if json.Unmarshal(proof, &prepared) != nil || prepared.Fingerprint != ch.Fingerprint(base) || len(prepared.Digest) != 64 {
		return errors.New("invalid prepared source proof")
	}
	fmt.Printf("Prepared disk sha256=%s hypervisor=%s\n", prepared.Digest, c.Hypervisor.SHA256)
	pool := l0Pool{config: c, configPath: configPath, repo: repo, base: base, egress: egress}
	binding := net.JoinHostPort(gateway.String(), "0")
	f, err := newFixture(ctx, binding, "http://"+binding, "l0-qualification", 3*time.Minute, peers...)
	if err != nil {
		return fmt.Errorf("start private JobDB/Git on %s: %w", binding, err)
	}
	defer f.Close()
	if nested {
		kitDir := filepath.Join(f.dir, "kit")
		args := []string{"scripts/kvm/qualify.py", "build", kitDir, "--arch", runtime.GOARCH}
		if cache != "" {
			abs, err := filepath.Abs(cache)
			if err != nil {
				return err
			}
			args = append(args, "--cache", abs)
		}
		if err := command(ctx, repo, "python3", args...); err != nil {
			return fmt.Errorf("build nested kit: %w", err)
		}
		pool.kit = filepath.Join(kitDir, "worker.tar.gz")
	}
	docker := slices.Contains(base.Capabilities, "docker")
	fmt.Printf("Assignment matrix: docker=%t kvm=%t\n", docker, nested)
	for _, test := range l0Cases(nested, docker) {
		if err := pool.runCase(ctx, f, test); err != nil {
			return fmt.Errorf("%s: %w", test.name, err)
		}
	}
	fmt.Printf("PASS L0 qualification linux/%s expected-kvm=%s\n", runtime.GOARCH, expect)
	return nil
}

func main() {
	repo := flag.String("repo", ".", "qualification checkout (set by the entry script)")
	config := flag.String("config", "", "dedicated provider settings (default: provider's normal config path)")
	expect := flag.String("expect-kvm", "supported", "expected nested KVM capability: supported or unsupported")
	egress := flag.String("egress-url", "", "HTTP 200 endpoint with a DNS name for the nested network test (required with supported KVM)")
	cache := flag.String("cache", "", "offline native nested kit inputs: kernel, initramfs and cloud-hypervisor")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: scripts/qualify-l0.sh [--config FILE] [--expect-kvm supported|unsupported] [--egress-url URL] [--cache DIR]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if *config == "" {
		*config, err = ch.DefaultConfigPath()
	} else {
		*config, err = filepath.Abs(*config)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err == nil {
		err = qualify(ctx, *repo, *config, *expect, *egress, *cache)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL L0 qualification:", err)
		os.Exit(1)
	}
}

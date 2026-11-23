package cloudhypervisor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/colony-2/pulse-cloudhypervisor/internal/images"
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

var ErrClosed = api.ErrProviderClosed
var ErrBatch = errors.New("cloudhypervisor: invalid launch batch")
var ErrToken = errors.New("cloudhypervisor: invalid page request or token")
var launchRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type terminal struct {
	Version                   int `json:"version"`
	LaunchID, Owner, Identity string
}
type Runtime struct {
	mu                sync.Mutex
	maintenance       sync.Mutex
	maintenanceCtx    context.Context
	maintenanceCancel context.CancelFunc
	building          bool
	config            Config
	lock              *os.File
	backend           native
	records           map[string]runner.Spec
	builders          map[string]runner.Spec
	completed         map[string]runner.Spec
	fences            map[string]bool
	subscriptions     []*subscription
	closed            bool
	draining          bool
	// Commit injection is local and never part of user configuration.
	writeHook func(string) error
	ready     func(Base) bool
	qualified func(Base) bool
}

var _ api.Provider = (*Runtime)(nil)

func key(id string) string { s := sha256.Sum256([]byte(id)); return hex.EncodeToString(s[:12]) }
func identity() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("cryptographic random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func New(config Config) (*Runtime, error) {
	c, e := Normalize(config)
	if e != nil {
		return nil, e
	}
	if e = state.PrivateDir(c.StateDir); e != nil {
		return nil, e
	}
	lock, e := state.Lock(filepath.Join(c.StateDir, "owner.lock"))
	if e != nil {
		return nil, e
	}
	p := &Runtime{config: c, lock: lock, records: map[string]runner.Spec{}, builders: map[string]runner.Spec{}, completed: map[string]runner.Spec{}, fences: map[string]bool{}}
	p.maintenanceCtx, p.maintenanceCancel = context.WithCancel(context.Background())
	p.backend = systemd{c}
	p.ready = p.sourceReady
	p.qualified = func(b Base) bool { _, e := p.sourceProof(b); return e == nil }
	if e = state.PrivateDir(filepath.Join(c.StateDir, "jobs")); e == nil {
		e = p.load()
		if e == nil {
			e = p.loadBuilders()
		}
	}
	if e != nil {
		lock.Close()
		return nil, e
	}
	return p, nil
}
func (p *Runtime) load() error {
	entries, e := os.ReadDir(filepath.Join(p.config.StateDir, "jobs"))
	if e != nil {
		return state.ErrCorrupt
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return state.ErrCorrupt
		}
		dir := filepath.Join(p.config.StateDir, "jobs", entry.Name())
		if state.PrivateDir(dir) != nil {
			return state.ErrCorrupt
		}
		if _, e = os.Lstat(filepath.Join(dir, "terminal.json")); e == nil {
			var f terminal
			if state.Read(filepath.Join(dir, "terminal.json"), &f) != nil || f.Version != 1 || f.Owner != p.config.Owner || key(f.LaunchID) != entry.Name() {
				return state.ErrCorrupt
			}
			p.fences[f.LaunchID] = true
			if _, err := os.Lstat(filepath.Join(dir, "record.json")); err == nil {
				s, err := runner.ReadSpec(dir)
				if err != nil || s.Identity != f.Identity || s.Owner != f.Owner || s.LaunchID != f.LaunchID {
					return state.ErrCorrupt
				}
				p.completed[s.LaunchID] = s
			} else if !os.IsNotExist(err) {
				return state.ErrCorrupt
			}
			continue
		}
		s, e := runner.ReadSpec(dir)
		if e != nil || s.Owner != p.config.Owner || key(s.LaunchID) != entry.Name() || !filepath.IsAbs(s.Source) || s.Slot < 0 {
			return state.ErrCorrupt
		}
		p.records[s.LaunchID] = s
		p.fences[s.LaunchID] = true
	}
	return nil
}
func (p *Runtime) dir(id string) string { return filepath.Join(p.config.StateDir, "jobs", key(id)) }
func (p *Runtime) sourceProof(b Base) (prepared, error) {
	var proof prepared
	if state.Read(filepath.Join(p.config.StateDir, "sources", Fingerprint(b)+".json"), &proof) != nil || !proof.matches(b) || proof.Path != filepath.Join(p.config.StateDir, "sources", Fingerprint(b)+"-"+proof.Digest[:min(12, len(proof.Digest))]+".raw") || len(proof.Digest) != 64 {
		return proof, ErrPreparation
	}
	return proof, nil
}
func (p *Runtime) sourceReady(b Base) bool {
	proof, e := p.sourceProof(b)
	return e == nil && images.Verify(proof.Path, proof.Digest) == nil
}

func (p *Runtime) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx == nil {
		return api.Capabilities{}, ErrBatch
	}
	if e := ctx.Err(); e != nil {
		return api.Capabilities{}, e
	}
	if p.closed {
		return api.Capabilities{}, ErrClosed
	}
	out := api.Capabilities{Bases: []api.BaseCapabilities{}, BasesComplete: true}
	// Qualified support is independent of occupied slots. This reads local proof
	// records only; there are no native mutations or warming operations.
	for _, b := range p.config.Bases {
		if nativeBase(b) && p.qualified(b) {
			out.Bases = append(out.Bases, api.BaseCapabilities{Name: b.Name, Capabilities: append([]string{}, b.Capabilities...)})
		} else if nativeBase(b) {
			out.BasesComplete = false
		}
	}
	return out, nil
}
func validate(ls []api.Launch) error {
	if len(ls) < 1 || len(ls) > api.MaxBatch {
		return ErrBatch
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if !launchRE.MatchString(l.LaunchID) || seen[l.LaunchID] || l.Lease == "" || len(l.Lease) > 512<<10 || l.Base == "" || len(l.Base) > 1024 || l.TimeoutSeconds < 0 || l.TimeoutSeconds > 31536000 {
			return ErrBatch
		}
		seen[l.LaunchID] = true
		for _, q := range []int64{l.CPUMillis, l.MemoryBytes, l.ScratchBytes} {
			if q < 0 || q > api.MaxQuantity {
				return ErrBatch
			}
		}
		if len(l.Capabilities) > 256 {
			return ErrBatch
		}
		for _, v := range l.Capabilities {
			if v == "" || len(v) > 128 || strings.ContainsAny(v, "\x00\n\r") {
				return ErrBatch
			}
		}
		u, e := url.Parse(l.Job.JobDB)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(l.Job.JobDB) > 4096 {
			return ErrBatch
		}
		for _, s := range []string{l.Job.InstanceID, l.Job.TenantID, l.Job.JobID} {
			if strings.TrimSpace(s) == "" || len(s) > 256 || strings.ContainsAny(s, "\x00\n\r") {
				return ErrBatch
			}
		}
	}
	return nil
}

// Match implements the API's only metacharacter, '*'. Every other byte is literal.
func Match(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(value, part)
		if i < 0 {
			return false
		}
		value = value[i+len(part):]
	}
	return strings.HasSuffix(value, parts[len(parts)-1])
}
func allOf(want, have []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if w == h {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (p *Runtime) register(ls []api.Launch, o api.SubmitOptions) *subscription {
	s := newSubscription(ls, o.Listener)
	if s == nil {
		return nil
	}
	kept := p.subscriptions[:0]
	for _, v := range p.subscriptions {
		select {
		case <-v.done:
		default:
			kept = append(kept, v)
		}
	}
	p.subscriptions = kept
	if len(kept) >= maxSubscriptions {
		s.finish(api.ListenerCapacityExceeded, api.ErrLogCapacity)
	} else {
		p.subscriptions = append(p.subscriptions, s)
	}
	return s
}
func (p *Runtime) Submit(ctx context.Context, ls []api.Launch, o api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	metadata := ls
	if len(metadata) > api.MaxBatch {
		metadata = metadata[:api.MaxBatch]
	}
	sub := p.register(metadata, o)
	var handle api.LogSubscription
	if sub != nil {
		handle = sub
	}
	if validate(ls) != nil || ctx == nil {
		if sub != nil {
			sub.finish(api.ListenerProtocolError, api.ErrLogProtocol)
		}
		return nil, handle, ErrBatch
	}
	out := make([]api.Submission, 0, len(ls))
	var methodErr error
	for _, l := range ls {
		result := api.Submission{LaunchID: l.LaunchID, Status: api.Unavailable, Reason: "provider is closed"}
		if p.closed {
			methodErr = ErrClosed
		} else if e := ctx.Err(); e != nil {
			result.Reason = "canceled before intent"
			methodErr = e
		} else {
			result, methodErr = p.admit(ctx, l, sub, methodErr)
		}
		out = append(out, result)
		if result.Status != api.Accepted {
			if sub != nil && result.Status == api.Unknown {
				sub.finish(api.ListenerSourceUnavailable, api.ErrLogSource)
			} else {
				sub.unavailable(l.LaunchID)
			}
		}
	}
	return out, handle, methodErr
}
func (p *Runtime) admit(ctx context.Context, l api.Launch, sub *subscription, prior error) (api.Submission, error) {
	result := api.Submission{LaunchID: l.LaunchID}
	decline := func(status api.Status, reason string) (api.Submission, error) {
		result.Status = status
		result.Reason = reason
		return result, prior
	}
	if p.fences[l.LaunchID] {
		return decline(api.Rejected, "launch identity already fenced")
	}
	if l.ScratchBytes > 0 || l.StartBefore != nil {
		return decline(api.Unsupported, "scratch or startup deadline is unsupported")
	}
	var selected *Base
	for i := range p.config.Bases {
		b := &p.config.Bases[i]
		if nativeBase(*b) && Match(l.Base, b.Name) && allOf(l.Capabilities, b.Capabilities) && l.CPUMillis <= b.UsableCPUMillis && l.MemoryBytes <= b.UsableMemoryBytes {
			selected = b
			break
		}
	}
	if selected == nil {
		return decline(api.Unsupported, "no qualified profile satisfies constraints")
	}
	if p.draining || !p.ready(*selected) {
		return decline(api.NoCapacity, "source is warming or capacity is draining")
	}
	slot := -1
	used := map[int]bool{}
	var cpu, memory, disk int64
	reservations := make([]runner.Spec, 0, len(p.records)+len(p.builders))
	for _, s := range p.records {
		reservations = append(reservations, s)
	}
	for _, s := range p.builders {
		reservations = append(reservations, s)
	}
	for _, s := range reservations {
		used[s.Slot] = true
		cpu += int64(s.VCPUs) * 1000
		memory += s.MemoryBytes
		disk += s.DiskBytes
	}
	for i := 0; i < *p.config.Count && i < len(p.config.Network); i++ {
		if !used[i] {
			slot = i
			break
		}
	}
	if slot < 0 || cpu+int64(p.config.VCPUs)*1000 > p.config.HostCPUMillis || memory+p.config.MemoryBytes > p.config.HostMemoryBytes || disk+p.config.DiskBytes > p.config.HostDiskBytes {
		return decline(api.NoCapacity, "reserved host budget exhausted")
	}
	if e := ctx.Err(); e != nil {
		result, _ = decline(api.Unavailable, "canceled before intent")
		return result, e
	}
	if e := p.backend.preflight(ctx, slot); e != nil {
		return decline(api.Unavailable, "native prerequisites unavailable")
	}
	boot, e := runner.BootID()
	if e != nil {
		return decline(api.Unavailable, "host boot identity unavailable")
	}
	b := *selected
	source, sourceDigest := p.preparedSource(b)
	n := p.config.Network[slot]
	id := identity()
	macBytes, _ := hex.DecodeString(id[:10])
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", macBytes[0], macBytes[1], macBytes[2], macBytes[3], macBytes[4])
	s := runner.Spec{Version: 1, Owner: p.config.Owner, LaunchID: l.LaunchID, Identity: id, Job: l.Job, Unit: "pulse-ch-" + key(p.config.StateDir) + "-" + key(l.LaunchID) + ".service", BootID: boot, Created: time.Now().UTC(), Slot: slot, Facts: api.Allocation{Base: b.Name, CPUMillis: b.UsableCPUMillis, MemoryBytes: b.UsableMemoryBytes, Capabilities: append([]string{}, b.Capabilities...)}, Platform: "linux/" + b.Architecture, Hypervisor: p.config.Hypervisor.Path, HypervisorDigest: p.config.Hypervisor.SHA256, Kernel: b.Kernel.Path, KernelDigest: b.Kernel.SHA256, Initramfs: b.Initramfs.Path, InitramfsDigest: b.Initramfs.SHA256, Source: source, SourceDigest: sourceDigest, Fingerprint: Fingerprint(b), VCPUs: p.config.VCPUs, MemoryBytes: p.config.MemoryBytes, DiskBytes: p.config.DiskBytes, TimeoutSeconds: l.TimeoutSeconds, Systemctl: p.config.Systemctl, UserManager: p.config.UserManager, TAP: n.TAP, Address: n.Address, Gateway: n.Gateway, Netmask: n.Netmask, DNS: n.DNS, MAC: mac, CID: uint32(slot + 3)}
	dir := p.dir(l.LaunchID)
	if e = state.PrivateDir(dir); e != nil {
		return decline(api.Unavailable, "private reservation unavailable")
	}
	// Retain even uncertain post-rename writes. No handoff occurs on any error.
	p.records[l.LaunchID] = s
	p.fences[l.LaunchID] = true
	f, lockErr := state.OpenPrivate(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_RDWR)
	if lockErr != nil {
		return decline(api.Unavailable, "runner lock unavailable")
	}
	f.Close()
	if e = state.Atomic(filepath.Join(dir, "record.json"), s, p.writeHook); e != nil {
		result.Status = api.Unavailable
		result.Reason = "reservation commit failed; no dispatch attempted"
		return result, e
	}
	accepted, e := p.backend.start(ctx, dir, s, l.Lease, sub)
	if accepted {
		result.Status = api.Accepted
		return result, prior
	}
	result.Status = api.Unknown
	result.Reason = "native handoff uncertain; do not replay"
	if e == nil {
		e = ErrObservation
	}
	return result, e
}
func (p *Runtime) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.maintenanceCancel()
	for _, s := range p.subscriptions {
		s.finish(api.ListenerProviderClosed, api.ErrProviderClosed)
	}
	return p.lock.Close()
}
func (p *Runtime) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrBatch
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if e := p.Reconcile(ctx); e != nil {
				return e
			}
		}
	}
}
func (p *Runtime) Reconcile(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if ctx == nil {
		return ErrBatch
	}
	if e := p.reconcileBuilders(ctx); e != nil {
		return e
	}
	cleanup := map[string]runner.Spec{}
	for id, s := range p.records {
		cleanup[id] = s
	}
	for id, s := range p.completed {
		cleanup[id] = s
	}
	for id, s := range cleanup {
		if e := ctx.Err(); e != nil {
			return e
		}
		dir := p.dir(id)
		o, e := p.backend.observe(ctx, dir, s)
		if e != nil {
			return e
		}
		if !o.terminated {
			continue
		}
		if err := p.reapTerminatedJob(id, dir, s); err != nil {
			return err
		}
	}
	return nil
}

func (p *Runtime) reapTerminatedJob(id, dir string, s runner.Spec) error {
	// Validate the whole private resource set before writing a terminal fence.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return state.ErrUnsafe
	}
	allowed := map[string]bool{"disk.raw": true, "handoff": true, "vsock": true, "api": true, "api.lock": true, "status.json": true, "record.json": true, "runner.lock": true, "start.fence": true, "terminal.json": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return state.ErrUnsafe
		}
	}
	lock, err := lockAPISidecar(dir)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}
	// A durable identity-only fence is committed before deleting owned files.
	if e := state.Atomic(filepath.Join(dir, "terminal.json"), terminal{1, id, p.config.Owner, s.Identity}, p.writeHook); e != nil {
		return e
	}
	if err := removeAPISidecar(dir, lock); err != nil {
		return err
	}
	for _, file := range []string{"disk.raw", "handoff", "vsock", "api", "status.json", "record.json"} {
		path := filepath.Join(dir, file)
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return state.ErrUnsafe
		}
		if os.Remove(path) != nil {
			return state.ErrStorage
		}
	}
	delete(p.records, id)
	delete(p.completed, id)
	return nil
}

func (p *Runtime) List(ctx context.Context, r api.ListRequest) (api.ListResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return api.ListResponse{}, ErrClosed
	}
	if ctx == nil {
		return api.ListResponse{}, ErrBatch
	}
	if e := ctx.Err(); e != nil {
		return api.ListResponse{}, e
	}
	if r.PageSize < 0 || r.PageSize > 100 || len(r.PageToken) > 16384 || (r.LaunchID != "" && !launchRE.MatchString(r.LaunchID)) {
		return api.ListResponse{}, ErrToken
	}
	if r.PageSize == 0 {
		r.PageSize = 100
	}
	type token struct{ Owner, Filter, After string }
	after := ""
	if r.PageToken != "" {
		data, e := base64.RawURLEncoding.DecodeString(r.PageToken)
		var t token
		if e != nil || json.Unmarshal(data, &t) != nil || t.Owner != key(p.config.StateDir) || t.Filter != r.LaunchID || !launchRE.MatchString(t.After) {
			return api.ListResponse{}, ErrToken
		}
		after = t.After
	}
	ids := []string{}
	for id := range p.records {
		if id > after && (r.LaunchID == "" || r.LaunchID == id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := api.ListResponse{Items: []api.Instance{}}
	for _, id := range ids {
		if len(out.Items) == r.PageSize {
			t := token{key(p.config.StateDir), r.LaunchID, out.Items[len(out.Items)-1].LaunchID}
			b, _ := json.Marshal(t)
			out.NextPageToken = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		s := p.records[id]
		o, e := p.backend.observe(ctx, p.dir(id), s)
		if e != nil {
			return api.ListResponse{}, e
		}
		if o.terminated {
			continue
		}
		if o.state == "" {
			o.state = "starting"
		}
		created := s.Created
		out.Items = append(out.Items, api.Instance{ID: s.Identity, LaunchID: id, Job: s.Job, State: o.state, Refs: []string{s.Unit}, CreatedAt: &created})
	}
	return out, nil
}

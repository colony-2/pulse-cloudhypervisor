// Package live holds the native acceptance fixture boundary. Descriptors contain
// nonsecret resource references only. A supplied assignment must arrive through
// an authenticated in-memory connection, never the descriptor or environment.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/protocol"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

var errFixture = errors.New("native fixture authority or inputs unavailable")

type fixture struct {
	Version        int      `json:"version"`
	Identity       string   `json:"identity"`
	ConfigPath     string   `json:"config"`
	ControlSocket  string   `json:"control_socket"`
	BootstrapAgent ch.Asset `json:"bootstrap_agent"`
	Faults         []string `json:"faults"`
}

// Fault declarations are prerequisites, not permission to mutate arbitrary host
// resources. Each fault test must also bind its action to its exact owned target.
// Reboot needs a separate external observer and grant; ordinary guest execution
// must not depend on that separate prerequisite.
func (f fixture) guestFaultAuthority() error {
	return f.requireFaults("controller-stop", "runner-kill")
}

func (f fixture) rebootFaultAuthority() error {
	return f.requireFaults("host-reboot")
}

func (f fixture) requireFaults(required ...string) error {
	for _, name := range required {
		found := false
		for _, declared := range f.Faults {
			found = found || declared == name
		}
		if !found {
			return errFixture
		}
	}
	return nil
}

func loadFixture(path string) (fixture, ch.Config, error) {
	var f fixture
	var c ch.Config
	if !filepath.IsAbs(path) {
		return f, c, errFixture
	}
	file, err := state.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return f, c, errFixture
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&f) != nil {
		return f, c, errFixture
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || f.Version != 1 || f.Identity == "" || len(f.Identity) > 200 || !filepath.IsAbs(f.ConfigPath) || !filepath.IsAbs(f.ControlSocket) || len(f.ControlSocket) > 100 || !filepath.IsAbs(f.BootstrapAgent.Path) || len(f.BootstrapAgent.SHA256) != 64 {
		return f, c, errFixture
	}
	for _, r := range f.Identity {
		if r < 33 || r > 126 {
			return f, c, errFixture
		}
	}
	c, err = ch.LoadConfig(f.ConfigPath)
	if err != nil {
		return f, c, errFixture
	}
	c, err = ch.Normalize(c)
	if err != nil {
		return f, c, errFixture
	}
	return f, c, nil
}

func (f fixture) connect(ctx context.Context) (*net.UnixConn, error) {
	return f.connectAuthenticated(ctx, protocol.SameUser)
}

func (f fixture) connectAuthenticated(ctx context.Context, authenticate func(*net.UnixConn) error) (*net.UnixConn, error) {
	// Reject a foreign endpoint before contacting it, then authenticate the peer.
	info, err := os.Lstat(f.ControlSocket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errFixture
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", f.ControlSocket)
	if err != nil {
		return nil, errFixture
	}
	unix, ok := conn.(*net.UnixConn)
	if !ok || authenticate(unix) != nil {
		conn.Close()
		return nil, errFixture
	}
	return unix, nil
}

// concurrentCapacity checks an already normalized profile without reserving a
// worker. Real memory, disk and KVM observations remain in native preflight.
func concurrentCapacity(c ch.Config, hostCPUs int) bool {
	return c.Count != nil && *c.Count >= 2 && len(c.Network) >= 2 &&
		hostCPUs >= 2*c.VCPUs && c.HostCPUMillis >= int64(2*c.VCPUs)*1000 &&
		c.HostMemoryBytes >= 2*c.MemoryBytes && c.HostDiskBytes >= 3*c.DiskBytes
}

// Every request authenticates the Unix peer and bounds time and body size. No
// fixture response/error is included in diagnostics: assignments carry leases.
func (f fixture) request(ctx context.Context, path string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{
		DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return f.connect(ctx) },
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 8192,
		ResponseHeaderTimeout:  5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://fixture"+path, nil)
	if err != nil {
		return errFixture
	}
	req.Header.Set("X-Pulse-Fixture", f.Identity)
	response, err := transport.RoundTrip(req)
	if err != nil {
		return errFixture
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return errFixture
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	defer clear(body)
	if err != nil || len(body) > 1<<20 {
		return errFixture
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil {
		return errFixture
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errFixture
	}
	return nil
}

func (f fixture) ready(ctx context.Context) error {
	var health struct {
		Ready bool `json:"ready"`
	}
	if f.request(ctx, "/health", &health) != nil || !health.Ready {
		return errFixture
	}
	return nil
}

type fixtureAssignment struct {
	Job   api.JobRef `json:"job"`
	Lease api.Lease  `json:"lease"`
}

func (fixtureAssignment) String() string     { return "fixture assignment (redacted)" }
func (a fixtureAssignment) GoString() string { return a.String() }

var fixtureCaseName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func validFixtureJob(j api.JobRef) bool {
	u, err := url.Parse(j.JobDB)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(j.JobDB) > 4096 {
		return false
	}
	for _, s := range []string{j.InstanceID, j.TenantID, j.JobID} {
		if strings.TrimSpace(s) == "" || len(s) > 256 {
			return false
		}
		for _, r := range s {
			if r < 33 || r == 127 {
				return false
			}
		}
	}
	return true
}

func (f fixture) assignment(ctx context.Context, name, variant string) (fixtureAssignment, error) {
	return f.assignmentProfile(ctx, name, variant, "renewal")
}

func (f fixture) assignmentProfile(ctx context.Context, name, variant, profile string) (fixtureAssignment, error) {
	switch profile {
	case "renewal", "docker", "isolation", "lifetime", "output":
	default:
		return fixtureAssignment{}, errFixture
	}
	if !fixtureCaseName.MatchString(name) || (variant != "valid" && variant != "expired" && variant != "mismatched" && variant != "completed") {
		return fixtureAssignment{}, errFixture
	}
	var a fixtureAssignment
	if f.request(ctx, "/launch?name="+name+"&case="+variant+"&profile="+profile, &a) != nil || !validFixtureJob(a.Job) || a.Lease == "" || len(a.Lease) > 512<<10 {
		return fixtureAssignment{}, errFixture
	}
	return a, nil
}

type fixtureStatus struct {
	Job                  api.JobRef `json:"job"`
	State                string     `json:"state"`
	Outcome              string     `json:"outcome"`
	OutputOK             bool       `json:"output_ok"`
	InitialExpiry        time.Time  `json:"initial_expiry"`
	LatestExpiry         time.Time  `json:"latest_expiry"`
	Acquisitions         int        `json:"acquisitions"`
	Renewals             int        `json:"renewals"`
	RenewedBeyondInitial bool       `json:"renewed_beyond_initial"`
	Chapters             int        `json:"chapters"`
	ClaimAttempts        int64      `json:"claim_attempts"`
}

func (f fixture) observe(ctx context.Context, name string) (fixtureStatus, error) {
	if !fixtureCaseName.MatchString(name) {
		return fixtureStatus{}, errFixture
	}
	var s fixtureStatus
	if f.request(ctx, "/status?name="+name, &s) != nil || !validFixtureJob(s.Job) || s.State == "" || s.InitialExpiry.IsZero() || s.LatestExpiry.Before(s.InitialExpiry) || s.Acquisitions < 0 || s.Renewals < 0 || s.ClaimAttempts < 0 {
		return fixtureStatus{}, errFixture
	}
	return s, nil
}

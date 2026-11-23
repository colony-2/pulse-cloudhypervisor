package consumer_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

var _ api.Provider = (*ch.Runtime)(nil)

func config(t *testing.T) (ch.Config, []byte) {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "consumer-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c := ch.Config{Version: 1, Owner: "external", StateDir: filepath.Join(dir, "owner")}
	data, _ := json.Marshal(c)
	return c, data
}
func exercise(t *testing.T, p api.Provider) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caps, e := p.GetCapabilities(ctx)
	if e != nil || !caps.BasesComplete || len(caps.Bases) != 0 {
		t.Fatal("discovery", e)
	}
	l := api.Launch{LaunchID: "external", Allocation: api.Allocation{Base: "ubuntu*"}, Job: api.JobRef{JobDB: "https://fixture.invalid/tenant", InstanceID: "instance", TenantID: "tenant", JobID: "job"}, Lease: "synthetic-consumer-lease"}
	rs, sub, e := p.Submit(ctx, []api.Launch{l}, api.SubmitOptions{Listener: &api.ListenOptions{Context: context.Background(), OnLog: func(context.Context, api.LaunchLog) error { return nil }}})
	if e != nil || len(rs) != 1 || rs[0].Status != api.Unsupported || sub == nil {
		t.Fatal("submission/log handle", e)
	}
	r, _ := sub.Wait(ctx)
	if len(r.Coverage) != 3 || r.Reason != api.ListenerSourceUnavailable {
		t.Fatal("log coverage", r.Reason)
	}
	page, e := p.List(ctx, api.ListRequest{})
	if e != nil || len(page.Items) != 0 {
		t.Fatal("list", e)
	}
}
func TestProcessHost(t *testing.T) {
	_, data := config(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, e := plugin.Start(ctx, plugin.StartOptions{Executable: os.Getenv("PROVIDER_BINARY"), Args: []string{"server"}, Init: plugin.Initialization{Name: "external", BaseDir: "/tmp", ConfigYAML: data}, StartupTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if p.Health().Protocol != 3 {
		t.Fatal("baseline")
	}
	exercise(t, p)
}

type trust struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	roots *x509.CertPool
}

func newTrust(t *testing.T) trust {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test authority"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	b, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca, e = x509.ParseCertificate(b)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return trust{ca, key, roots}
}
func (tr trust) pair(t *testing.T, name string, client bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, e := x509.CreateCertificate(rand.Reader, cert, tr.ca, &key.PublicKey, tr.key)
	if e != nil {
		t.Fatal(e)
	}
	kb, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	cb, kp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
	pair, e := tls.X509KeyPair(cb, kp)
	if e != nil {
		t.Fatal(e)
	}
	return pair, cb, kp
}
func TestAuthenticatedOutboundHostAndUnauthorizedIdentity(t *testing.T) {
	tr := newTrust(t)
	server, _, _ := tr.pair(t, "collector.test", false)
	_, cert, key := tr.pair(t, "external", true)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	collector, e := plugin.StartCollector(ctx, l, plugin.CollectorOptions{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}, ClientCAs: tr.roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{plugin.ReverseALPN}}, Authorize: func(_ context.Context, s tls.ConnectionState) (string, error) {
		if s.PeerCertificates[0].Subject.CommonName != "external" {
			return "", errors.New("unauthorized")
		}
		return "external", nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer collector.Close()
	cfg, _ := config(t)
	root := filepath.Dir(cfg.StateDir)
	path := filepath.Join(root, "config.json")
	if ch.SaveConfig(path, cfg) != nil {
		t.Fatal("settings")
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tr.ca.Raw})
	for name, b := range map[string][]byte{"ca.pem": ca, "client.pem": cert, "key.pem": key} {
		if os.WriteFile(filepath.Join(root, name), b, 0600) != nil {
			t.Fatal("credentials")
		}
	}
	args := []string{"server", "--config", path, "--outbound", l.Addr().String(), "--name", "external", "--ca", filepath.Join(root, "ca.pem"), "--cert", filepath.Join(root, "client.pem"), "--key", filepath.Join(root, "key.pem"), "--server-name", "collector.test"}
	cmd := exec.CommandContext(ctx, os.Getenv("PROVIDER_BINARY"), args...)
	if cmd.Start() != nil {
		t.Fatal("outbound command")
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	snapshot, e := collector.WaitForChange(ctx, 1)
	if e != nil || len(snapshot.Providers) != 1 {
		t.Fatal("outbound admission", e)
	}
	exercise(t, snapshot.Providers[0].Provider)
	// Killing transport closes the session without business-operation replay.
	if collector.Close() != nil {
		t.Fatal("collector close")
	}
	if _, e = snapshot.Providers[0].Provider.List(ctx, api.ListRequest{}); e == nil {
		t.Fatal("lost transport remained usable")
	}
	// An unrecognized client certificate must fail before factory construction.
	bad, _, _ := tr.pair(t, "foreign", true)
	l2, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	co, e := plugin.StartCollector(ctx, l2, plugin.CollectorOptions{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}, ClientCAs: tr.roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{plugin.ReverseALPN}}, Authorize: func(context.Context, tls.ConnectionState) (string, error) { return "", errors.New("unauthorized") }})
	if e != nil {
		t.Fatal(e)
	}
	defer co.Close()
	called := false
	e = plugin.Run(ctx, func(context.Context, plugin.Init) (api.Provider, plugin.Cleanup, error) {
		called = true
		return nil, nil, errors.New("must not construct")
	}, plugin.RunOptions{Address: l2.Addr().String(), Init: plugin.Initialization{Name: "foreign", BaseDir: "/tmp", ConfigYAML: []byte(`{}`)}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: tr.roots, ServerName: "collector.test", Certificates: []tls.Certificate{bad}, NextProtos: []string{plugin.ReverseALPN}}})
	if e == nil || called {
		t.Fatal("unauthorized identity reached provider")
	}
}
func TestIncompatibleProcessBaselineRejected(t *testing.T) {
	// A deliberately incompatible executable advertises protocol 2. It cannot
	// negotiate baseline 3 or reach the provider constructor.
	path := filepath.Join(t.TempDir(), "old-provider")
	if os.WriteFile(path, []byte("#!/bin/sh\nprintf '1|2|unix|/nonexistent|grpc|\\n'\nexec /bin/sleep 3\n"), 0700) != nil {
		t.Fatal("fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, e := plugin.Start(ctx, plugin.StartOptions{Executable: path, Init: plugin.Initialization{Name: "external", BaseDir: "/tmp"}, StartupTimeout: time.Second})
	if p != nil {
		p.Close()
	}
	if e == nil {
		t.Fatal("baseline downgrade accepted")
	}
}

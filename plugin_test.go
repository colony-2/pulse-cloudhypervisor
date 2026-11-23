package cloudhypervisor

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

func TestSDKTransportLossDoesNotReplayAcceptedWork(t *testing.T) {
	p, f := testRuntime(t)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	key := bytes.Repeat([]byte{'x'}, 32)
	// Plain HTTP is limited to this loopback synthetic-key fixture. Production
	// command tests exercise certificate-authenticated TLS outbound hosting.
	collector, e := plugin.StartCollector(ctx, listener, plugin.CollectorOptions{AuthMode: plugin.ReverseAuthMutualKey, Transport: plugin.ReverseTransportHTTP, MutualKeys: map[string][]byte{"fixture": key}})
	if e != nil {
		t.Fatal(e)
	}
	defer collector.Close()
	runDone := make(chan error, 1)
	go func() {
		runDone <- plugin.Run(ctx, func(context.Context, plugin.Init) (api.Provider, plugin.Cleanup, error) {
			return p, func(context.Context) error { return p.Close() }, nil
		}, plugin.RunOptions{Address: listener.Addr().String(), AuthMode: plugin.ReverseAuthMutualKey, Transport: plugin.ReverseTransportHTTP, MutualKey: key, Init: plugin.Initialization{Name: "fixture", BaseDir: "/tmp", ConfigYAML: []byte(`{}`)}})
	}()
	snapshot, e := collector.WaitForChange(ctx, 1)
	if e != nil || len(snapshot.Providers) != 1 {
		t.Fatal("SDK session", e)
	}
	host := snapshot.Providers[0].Provider
	seen := make(chan api.LaunchLog, 1)
	rs, sub, e := host.Submit(ctx, []api.Launch{launch("sdk-accepted")}, api.SubmitOptions{Listener: options(func(_ context.Context, l api.LaunchLog) error { seen <- l; return nil })})
	if e != nil || len(rs) != 1 || rs[0].Status != api.Accepted || sub == nil {
		t.Fatal("SDK admission", e)
	}
	p.mu.Lock()
	local := p.subscriptions[len(p.subscriptions)-1]
	p.mu.Unlock()
	local.emit("sdk-accepted", api.LogSourceStdout, []byte{0, 255, 10})
	select {
	case l := <-seen:
		if !bytes.Equal(l.Data, []byte{0, 255, 10}) {
			t.Fatal("SDK binary record changed")
		}
	case <-ctx.Done():
		t.Fatal("SDK log not delivered")
	}
	if collector.Close() != nil {
		t.Fatal("collector close")
	}
	r := waitSub(t, sub)
	for _, source := range r.Coverage {
		if source.State == api.SourceActive || source.State == api.SourcePending {
			t.Fatal("transport loss failed to interrupt coverage")
		}
	}
	select {
	case <-runDone:
	case <-ctx.Done():
		t.Fatal("provider SDK cleanup did not finish")
	}
	if f.count() != 1 || len(p.records) != 1 {
		t.Fatal("SDK loss canceled or replayed accepted execution")
	}
	if _, e = host.List(ctx, api.ListRequest{}); e == nil {
		t.Fatal("old SDK generation remained usable")
	}
}

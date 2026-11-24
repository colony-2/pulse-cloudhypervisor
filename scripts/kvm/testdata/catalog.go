// Inspect the built provider over its SDK interface. A zero-count override
// exercises matching while ensuring the synthetic submission cannot dispatch.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

func check(binary, config, expect string) error {
	if expect != "supported" && expect != "unsupported" {
		return fmt.Errorf("choose --expect supported or unsupported")
	}
	c, err := ch.LoadConfig(config)
	if err != nil {
		return err
	}
	zero := 0
	c.Count = &zero
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := plugin.Start(ctx, plugin.StartOptions{Executable: binary, Args: []string{"server"}, Init: plugin.Initialization{Name: c.Owner, BaseDir: filepath.Dir(config), ConfigYAML: data}})
	if err != nil {
		return err
	}
	defer client.Close()
	caps, err := client.GetCapabilities(ctx)
	if err != nil {
		return err
	}
	native := "ubuntu-24.04-" + runtime.GOARCH
	if !caps.BasesComplete || len(caps.Bases) != 1 || caps.Bases[0].Name != native {
		return fmt.Errorf("prepare the candidate Ubuntu base first, catalog %+v", caps)
	}
	want := expect == "supported"
	advertised := caps.Bases[0].Capabilities
	wantDocker := false
	for _, b := range c.Bases {
		if b.Name == native {
			wantDocker = slices.Contains(b.Capabilities, "docker")
		}
	}
	fmt.Printf("candidate catalog %s %v\n", caps.Bases[0].Name, advertised)
	if slices.Contains(advertised, "kvm") != want {
		return fmt.Errorf("unexpected KVM advertisement")
	}
	if slices.Contains(advertised, "docker") != wantDocker {
		return fmt.Errorf("unexpected Docker advertisement for the prepared base")
	}
	for i, requested := range [][]string{nil, {"kvm"}, {"docker"}, {"docker", "kvm"}} {
		status := api.NoCapacity
		if slices.Contains(requested, "kvm") && !want || slices.Contains(requested, "docker") && !wantDocker {
			status = api.Unsupported
		}
		l := api.Launch{LaunchID: fmt.Sprintf("manual-catalog-%d-%d", time.Now().UnixNano(), i), Allocation: api.Allocation{Base: native, Capabilities: requested}, Job: api.JobRef{JobDB: "http://fixture.invalid/test", InstanceID: "test", TenantID: "test", JobID: "test"}, Lease: "synthetic-catalog-check"}
		out, _, err := client.Submit(ctx, []api.Launch{l}, api.SubmitOptions{})
		if err != nil {
			return err
		}
		if len(out) != 1 || out[0].Status != status {
			return fmt.Errorf("requested %v got %+v, expected %s", requested, out, status)
		}
		fmt.Printf("PASS request %v returned %s with drained capacity\n", requested, status)
	}
	return nil
}
func main() {
	provider := flag.String("provider", "", "absolute candidate provider binary")
	config := flag.String("config", "", "absolute provider configuration")
	expect := flag.String("expect", "", "supported or unsupported")
	flag.Parse()
	if err := check(*provider, *config, *expect); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

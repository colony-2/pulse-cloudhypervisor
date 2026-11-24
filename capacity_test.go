package cloudhypervisor

import (
	"fmt"
	"runtime"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestWorkerSizingDefaultsLimitsAndReservations(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct{ cpu, mem, wantCPU, wantMem int64 }{
		{0, 0, 2000, 768 * mib}, {1000, 0, 1000, 768 * mib}, {0, 512 * mib, 2000, 512 * mib},
		{1000, 512 * mib, 1000, 512 * mib}, {2200, 1024 * mib, 2200, 1024 * mib},
	} {
		t.Run(fmt.Sprintf("%dm-%dB", tc.cpu, tc.mem), func(t *testing.T) {
			p, f := testRuntime(t)
			p.config.DefaultWorkerCPUMillis, p.config.DefaultWorkerMemoryBytes = 1500, 768*mib
			p.config.MaxWorkerCPUMillis, p.config.MaxWorkerMemoryBytes = 2200, 1024*mib
			l := launch("sized")
			l.CPUMillis, l.MemoryBytes = tc.cpu, tc.mem
			if got := submit(t, p, l); got.Status != api.Accepted {
				t.Fatal(got)
			}
			s := f.starts[0]
			if s.Facts.CPUMillis != tc.wantCPU || s.Facts.MemoryBytes != tc.wantMem || int64(s.VCPUs) != (tc.wantCPU+999)/1000 || s.MemoryBytes != tc.wantMem+1024*mib {
				t.Fatalf("invalid dispatch %#v", s)
			}
			for _, resource := range []string{"cpu", "memory"} {
				l.LaunchID = resource
				l.CPUMillis, l.MemoryBytes = 2200, 1024*mib
				if resource == "cpu" {
					l.CPUMillis++
				} else {
					l.MemoryBytes++
				}
				if got := submit(t, p, l); got.Status != api.Unsupported {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestMixedSizeCapacityAfterRestart(t *testing.T) {
	for _, exhausted := range []string{"count", "cpu", "memory"} {
		t.Run(exhausted, func(t *testing.T) {
			p, _ := testRuntime(t)
			c := p.config
			count, total := 3, 3
			c.Count, c.TotalCapacity = &count, &total
			for i := 1; i < count; i++ {
				n := c.Network[0]
				n.TAP = fmt.Sprint("test", i)
				n.Address = fmt.Sprintf("192.0.2.%d", 2+i)
				c.Network = append(c.Network, n)
			}
			c.HostCPUMillis = 6000
			c.HostMemoryBytes = 12 << 30
			c.HostDiskBytes = 96 << 30
			switch exhausted {
			case "count":
				total = 2
			case "cpu":
				c.HostCPUMillis = 3000
			case "memory":
				c.HostMemoryBytes = 5 << 30
			}
			p.config = c
			if got := submit(t, p, launch("small")); got.Status != api.Accepted {
				t.Fatal(got)
			}
			large := launch("large")
			large.CPUMillis = 2000
			large.MemoryBytes = 2 << 30
			if got := submit(t, p, large); got.Status != api.Accepted {
				t.Fatal(got)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			f := &fakeNative{}
			reopened.backend = f
			reopened.ready = func(Base) bool { return true }
			if got := submit(t, reopened, launch("waiting")); got.Status != api.NoCapacity {
				t.Fatal(got)
			}
			f.observation.terminated = true
			if err := reopened.Reconcile(bg); err != nil {
				t.Fatal(err)
			}
			large.LaunchID = "next"
			if got := submit(t, reopened, large); got.Status != api.Accepted {
				t.Fatal(got)
			}
		})
	}
}

func TestCatalogAndAdmissionOnlyUseHostArchitecture(t *testing.T) {
	p, f := testRuntime(t)
	foreign := p.config.Bases[0]
	foreign.Architecture = "amd64"
	if runtime.GOARCH == "amd64" {
		foreign.Architecture = "arm64"
	}
	foreign.Name = "ubuntu-24.04-" + foreign.Architecture
	p.config.Bases = append([]Base{foreign}, p.config.Bases...)
	if _, err := Normalize(p.config); err != nil {
		t.Fatal(err)
	}
	cat, err := p.GetCapabilities(bg)
	if err != nil || !cat.BasesComplete || len(cat.Bases) != 1 || cat.Bases[0].Name != "ubuntu-24.04-"+runtime.GOARCH {
		t.Fatal(cat, err)
	}
	l := launch("foreign")
	l.Base = foreign.Name
	if got := submit(t, p, l); got.Status != api.Unsupported || f.count() != 0 {
		t.Fatal(got)
	}
	l = launch("native")
	if got := submit(t, p, l); got.Status != api.Accepted || f.starts[0].Platform != "linux/"+runtime.GOARCH {
		t.Fatal(got)
	}
}

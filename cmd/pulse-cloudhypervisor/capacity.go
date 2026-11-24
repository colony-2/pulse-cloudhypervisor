package main

import (
	"flag"
	"strconv"

	ch "github.com/colony-2/pulse-cloudhypervisor"
)

// Apply only explicitly supplied flags, including a zero worker count or budget.
func capacityFlags(f *flag.FlagSet) func(*ch.Config) {
	var overrides []func(*ch.Config)
	add := func(name, alias, help string, set func(*ch.Config, int64)) {
		parse := func(value string) error {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return ch.ErrConfig
			}
			overrides = append(overrides, func(c *ch.Config) { set(c, n) })
			return nil
		}
		f.Func(name, help, parse)
		if alias != "" {
			f.Func(alias, help, parse)
		}
	}
	add("linux-capacity", "l", "maximum concurrent Linux workers", func(c *ch.Config, n int64) { v := int(n); c.Count = &v })
	add("total-capacity", "c", "maximum concurrent workers across the pool", func(c *ch.Config, n int64) { v := int(n); c.TotalCapacity = &v })
	add("total-cpu-millis", "", "pool CPU budget in millicores", func(c *ch.Config, n int64) { c.HostCPUMillis = n })
	add("total-memory-bytes", "", "pool memory budget in bytes", func(c *ch.Config, n int64) { c.HostMemoryBytes = n })
	add("max-worker-cpu-millis", "", "maximum worker-usable CPU in millicores", func(c *ch.Config, n int64) { c.MaxWorkerCPUMillis = n })
	add("max-worker-memory-bytes", "", "maximum worker-usable memory in bytes", func(c *ch.Config, n int64) { c.MaxWorkerMemoryBytes = n })
	add("default-worker-cpu-millis", "", "worker CPU when omitted, in millicores", func(c *ch.Config, n int64) { c.DefaultWorkerCPUMillis = n })
	add("default-worker-memory-bytes", "", "worker memory when omitted, in bytes", func(c *ch.Config, n int64) { c.DefaultWorkerMemoryBytes = n })
	return func(c *ch.Config) {
		for _, apply := range overrides {
			apply(c)
		}
	}
}

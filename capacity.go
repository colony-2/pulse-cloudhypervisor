package cloudhypervisor

// Worker limits and defaults describe usable resources. The prepared profile
// supplies the upper bound and the headroom to retain for the guest OS.
type workerSize struct {
	vcpus                   int
	memoryBytes             int64
	usableCPU, usableMemory int64
}

func (c Config) workerCount() int {
	if c.TotalCapacity != nil {
		return min(*c.Count, *c.TotalCapacity)
	}
	return *c.Count
}

func limit(base, configured int64) int64 {
	if configured > 0 {
		return min(base, configured)
	}
	return base
}

func requestedSize(request, fallback, maximum int64) int64 {
	if request != 0 {
		return request
	}
	if fallback != 0 {
		return fallback
	}
	return maximum
}

func (c Config) sizeFor(b Base, cpu, memory int64) (workerSize, bool) {
	maxCPU := limit(b.UsableCPUMillis, c.MaxWorkerCPUMillis)
	maxMemory := limit(b.UsableMemoryBytes, c.MaxWorkerMemoryBytes)
	cpu = requestedSize(cpu, c.DefaultWorkerCPUMillis, maxCPU)
	memory = requestedSize(memory, c.DefaultWorkerMemoryBytes, maxMemory)
	if cpu <= 0 || cpu > maxCPU || memory <= 0 || memory > maxMemory {
		return workerSize{}, false
	}
	cpuReserve := int64(c.VCPUs)*1000 - b.UsableCPUMillis
	memoryReserve := c.MemoryBytes - b.UsableMemoryBytes
	vcpus := (cpu + cpuReserve + 999) / 1000
	const alignment = 2 << 20
	rounded := (memory + memoryReserve + alignment - 1) / alignment * alignment
	vmMemory := min(c.MemoryBytes, max(int64(1<<30), rounded))
	return workerSize{
		vcpus: int(vcpus), memoryBytes: vmMemory,
		usableCPU:    min(maxCPU, vcpus*1000-cpuReserve),
		usableMemory: min(maxMemory, vmMemory-memoryReserve),
	}, true
}

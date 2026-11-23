# Qualify ARM64 EL2 for provider-reboot testing

Please test `kvm-arm.mode=nested` on a disposable copy of the deployed Ubuntu ARM64 worker image, with the three qualified GIC trap arguments enabled. Job `3MUMNt3TrDzumi5EOfs8s0PAFwL` now boots Ubuntu workload VMs and runs Docker. Its provider-reboot test needs one more virtualization level. The worker must run a disposable provider VM that can start a workload VM through its own KVM device. [Docker and lease verification](http://db.lab.c2.run:8081/project/c2/jobs/3MWRVAwuLemKgzUPTdW2rGHPSCz/story)

On a fresh ordinary allocation, Ubuntu kernel `7.0.0-42-generic` had all three GIC traps, no explicit `kvm-arm.mode` argument, KVM API version 12, and `KVM_CAP_ARM_EL2=0`. Cloud Hypervisor v55 documents VHE and FEAT_NV2 as prerequisites for the ARM64 nested mode. Whether the Apple virtual CPU exposes the required features to this worker is still open. [Deployed worker probe](http://db.lab.c2.run:8081/project/c2/jobs/3MWUYi0KZTuYXTAdaQX6YYs0aax/story) · [Cloud Hypervisor v55 CPU requirements](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v55.0/docs/cpu.md#kvm-aarch64-host-requirements)

1. Cold-boot the canary worker. Record the image identity, kernel release, worker boot ID, effective KVM mode and GIC arguments. Capture relevant kernel initialization messages about VHE, nested virtualization or missing features.
2. As the normal job user, query `KVM_GET_API_VERSION` and `KVM_CHECK_EXTENSION(KVM_CAP_ARM_EL2)`. Record both integers. Re-run the existing virtio-disk reproducer to check ordinary guest I/O.
3. If the capability is positive, boot a disposable ARM64 provider VM with Cloud Hypervisor v55 and `--cpus boot=2,nested=on`. Inside it, use KVM to start a workload VM and capture a guest output marker. Return the image and worker identities, both capability results, and that marker.

If the kernel declines nested mode or the capability stays zero, please return the canary evidence and the reason reported by the kernel. The provider team can then take the placement question back to Direction. [Linux kernel parameter reference](https://docs.kernel.org/admin-guide/kernel-parameters.html)

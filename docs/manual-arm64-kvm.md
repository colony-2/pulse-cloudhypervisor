# Qualify guest KVM on an ARM64 Linux L0 host

Run these manual checks on the physical Linux machine that will host Cloud Hypervisor. L0 runs the provider, L1 is its disposable Ubuntu worker, and L2 is the VM the job starts inside that worker. There are two independently useful results. The scratch test measures the hardware, kernel, and VMM combination today. The assignment test qualifies a candidate provider and prepared base through the real c2j launch path once guest KVM is implemented. Record both results with the host kernel, provider commit, base fingerprint, and Cloud Hypervisor version.

## What pulse-macvz needed

| Change | What this provider needs to establish |
| --- | --- |
| The VZ runner queried host nested-virtualization support and enabled it for Linux ARM64 guests. Readiness opened `/dev/kvm` read/write, checked API version 12, and created a VM as the job user. | Query `KVM_CAP_ARM_EL2` on L0. Pass `--cpus boot=N,nested=on` to the **outer** Cloud Hypervisor for qualified builders and workers. Check the real c2j process credentials in L1. Our launcher sets UID/GID 1000 directly, so adding a user to a supplementary group in the image alone is insufficient. [macvz implementation](https://github.com/colony-2/pulse-macvz/commit/8d0ff8d) · [v55 host prerequisites](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v55.0/docs/cpu.md#kvm-aarch64-host-requirements) · [Current guest launcher](../internal/guest/agent.go) |
| To develop this provider inside a VZ worker, jobs needed dynamic TAPs, a per-allocation bridge and address descriptor, peer connectivity, DNS and outbound NAT. They also needed a lingering user systemd manager, a working D-Bus session and a helper that grants network administration inside a user service. macvz checked this with Docker both disabled and enabled. | Supply that environment inside each qualified L1. Prepare it before handoff, publish the boot-bound descriptor via `PULSE_KVM_NETWORK`, and propagate the user-manager environment to jobs. Generalize this repo’s native fixture, which currently hardcodes macvz’s descriptor path and bridge alias. [macvz networking and supervision](https://github.com/colony-2/pulse-macvz/commit/fcfcf56) · [Current fixture](../scripts/managed_fixture.py) |
| KVM API checks passed on the Mac while real nested virtio disk operations stalled. macvz enabled the three ARM GICv3 register traps in the **worker kernel**, verified the booted settings, and ran three fresh Cloud Hypervisor boots with PCI/MSI-X disk I/O. Updated image revisions applied to new or unused workers. | Test the Linux L0 system at its defaults first. If needed, repeat with the flags on the disposable L1 kernel. Cloud Hypervisor boots that kernel directly, so any later provider policy belongs in its kernel arguments and image qualification. Record results for each mode before choosing a default. [macvz fix](https://github.com/colony-2/pulse-macvz/commit/2044dd3) · [Three-boot evidence](https://github.com/colony-2/pulse-macvz/blob/2044dd3/docs/gic-traps-qualification.md) |

## 1. Check the host and run the scratch test

Run from the repository root as the Linux account that will run the VMM. Use ARM64 Linux with a nested-capable CPU and kernel, Python 3, curl, Go, and a native C compiler with static libc. Allow 4 GiB of allocatable guest RAM plus host overhead. The build downloads the pinned Ubuntu Noble kernel/initrd and upstream v55 static VMM and checks all three SHA-256 digests. The test compiles its small guest programs from this repository.

```sh
python3 scripts/kvm/qualify.py host
python3 scripts/kvm/qualify.py build /var/tmp/ch-kvm-kit
python3 scripts/kvm/qualify.py platform /var/tmp/ch-kvm-kit
```

The preflight prints the host kernel, account, KVM API version and EL2 capability. If EL2 is zero, check the L0 CPU for FEAT_NV2 and the host kernel for VHE and nested KVM mode. The upstream kernel option is `kvm-arm.mode=nested`. Host configuration changes and reboots are operator decisions. The test uses scratch disks and leaves host boot, network and service settings alone. [Cloud Hypervisor ARM64 requirements](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v55.0/docs/cpu.md#kvm-aarch64-host-requirements)

The platform test boots a diagnostic L1 with nested virtualization enabled. Its init launches three fresh L2 VMs via KVM. Each L2 gets a blank 64 MiB disk and performs 1,536 direct reads and 96 write/sync/readback cycles through the default PCI virtio block device. A pass requires successful VMM exits and all three guest completion markers before the deadline. This uses the disk workload from the original GIC investigation. [Disk workload source](../scripts/kvm/testdata/virtio-init.c)

For comparison, this command adds `kvm-arm.vgic_v3_group0_trap=1`, `kvm-arm.vgic_v3_group1_trap=1`, and `kvm-arm.vgic_v3_common_trap=1` to the scratch L1 only. Keep the default and trap-enabled console output separately. A pass with traps alone identifies the qualified configuration.

```sh
python3 scripts/kvm/qualify.py platform /var/tmp/ch-kvm-kit --gic-traps
```

To build offline, pass `--cache DIR` to `build` with the original pinned files named `kernel`, `initramfs`, and `cloud-hypervisor`. Use a fresh kit directory for each build.

## 2. Test the real worker on the same L0 host

Use a dedicated test pool routed to the candidate provider. Its assigned L1 must have enough RAM for a 1,536 MiB nested guest plus its own OS and c2j. The current provider advertises Docker only. Run this step after the candidate can admit a `kvm` assignment. Supply the resulting worker archive as a job artifact. The probes execute as the ordinary c2j UID through its actual launch environment.

```sh
c2j submit 'Qualify guest KVM on the Linux L0 candidate' \
  --jobdb "$JOBDB" --cell "$CELL" --require-base ubuntu-24.04-arm64 \
  --require-capability kvm --advanced-recipe-file scripts/kvm/worker-job.yaml \
  --artifact worker.tar.gz=/var/tmp/ch-kvm-kit/worker.tar.gz --json
```

The recipe first opens KVM and creates a VM as UID/GID 1000, then repeats the three disk boots. For the networking run add `--inputs-json '{"egress_url":"http://your-test-host.example/probe"}'` using a reachable DNS name that returns HTTP 200 without redirects or credentials. This starts two L2s on dynamic TAPs. One runs in an independently supervised user service. The other must reach the peer and L1 HTTP servers, resolve the external hostname through the advertised DNS server, and fetch the endpoint. For the combined run also request `--require-capability docker` and set `docker_image` in the same JSON to a preloaded `sha256:…` image containing `/bin/sh`. This also checks access to the Docker engine and passed-through KVM device from a user service.

```sh
c2j job result "$JOB_ID" --jobdb "$JOBDB" --field /exit_code --output -
c2j job result "$JOB_ID" --jobdb "$JOBDB" --field /observation --output -
python3 scripts/kvm/qualify.py match "$STATE_DIR" "$LAUNCH" "$IDENTITY"
```

Require exit code zero, three disk passes, and the networking pass when requested. Copy `launch` and `identity` from the worker output into the last command and run it on L0 as the provider owner. It binds that result to the chosen pool’s live record or terminal fence. This distinguishes a job serviced by another KVM-capable worker. On failure inspect the command task’s stderr with `c2j job log`. Keep the worker result and identity match alongside the L0 platform result before enabling the capability for that host/base combination.

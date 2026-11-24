# Qualify guest KVM on an ARM64 Linux L0 host

Run these manual checks on the physical Linux machine that will host Cloud Hypervisor. L0 runs the provider, L1 is its disposable Ubuntu worker, and L2 is the VM the job starts inside that worker. Qualification uses the candidate provider binary, its pinned guest agent, and real c2j assignments. Run the supported case on your nested-capable L0 machine. The same catalog test accepts an expected unsupported result on the macOS-backed Linux worker. Record the kernel, candidate commit, prepared base fingerprint and launch identity for each environment.

## What pulse-macvz needed

| Change | What this provider needs to establish |
| --- | --- |
| The VZ runner queried host nested-virtualization support and enabled it for Linux ARM64 guests. Readiness opened `/dev/kvm` read/write, checked API version 12, and created a VM as the job user. | Setup checks `KVM_CAP_ARM_EL2` on the host. After ordinary preparation it boots a disposable clone with `--cpus boot=N,nested=on` and checks VM creation and ARM vCPU initialization as UID/GID 1000. KVM assignments use the same CPU setting and guest setup. [macvz implementation](https://github.com/colony-2/pulse-macvz/commit/8d0ff8d) · [v55 host prerequisites](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v55.0/docs/cpu.md#kvm-aarch64-host-requirements) · [Current guest launcher](../internal/guest/agent.go) |
| To develop this provider inside a VZ worker, jobs needed dynamic TAPs, a per-allocation bridge and address descriptor, peer connectivity, DNS and outbound NAT. They also needed a lingering user systemd manager, a working D-Bus session and a helper that grants network administration inside a user service. macvz checked this with Docker both disabled and enabled. | The guest agent configures a private bridge, forwarding and DNS, starts the worker user manager, publishes `PULSE_KVM_NETWORK`, and verifies access from the job account and a transient user service. These operations run inside the disposable probe or KVM assignment. [macvz networking and supervision](https://github.com/colony-2/pulse-macvz/commit/fcfcf56) · [Current fixture](../scripts/managed_fixture.py) |
| KVM API checks passed on the Mac while real nested virtio disk operations stalled. macvz enabled the three ARM GICv3 register traps in the **worker kernel**, verified the booted settings, and ran three fresh Cloud Hypervisor boots with PCI/MSI-X disk I/O. Updated image revisions applied to new or unused workers. | Test the candidate at its defaults. To compare the three macvz trap settings, change `kvm_gic_traps` in the test pool settings, run setup again, and repeat the same assignment. The probe and the actual KVM worker receive identical kernel arguments. [macvz fix](https://github.com/colony-2/pulse-macvz/commit/2044dd3) · [Three-boot evidence](https://github.com/colony-2/pulse-macvz/blob/2044dd3/docs/gic-traps-qualification.md) |

## 1. Test the candidate's catalog and admission

Build the host and guest binaries from the same revision. Include that guest agent in the pinned boot assets and tool manifest, and point the test pool at the candidate runner. Use a dedicated owner with a configured Ubuntu base, TAPs and disk budget as described in the operations guide. The existing provider manages one owner at a time. Run setup and the catalog check with its server stopped. [Provider setup](operations.md)

```sh
"$PROVIDER" setup --config "$CONFIG"
go run scripts/kvm/testdata/catalog.go --provider "$PROVIDER" --config "$CONFIG" --expect supported
```

Use absolute paths for `PROVIDER` and `CONFIG`. Choose `--expect unsupported` where the host cannot expose another level of virtualization. The test starts the candidate binary through the plugin SDK using the prepared configuration with an in-memory zero-capacity override. It checks that ordinary, Docker, KVM and combined requests receive the expected matching decision. A supported request returns `no_capacity` in this drained test. An unsupported request returns `unsupported`. No test lease is executed or saved.

## 2. Run assignments through that provider

Start the candidate server with the saved pool configuration and route the test jobs to it. On either host, submit a regular job and optionally request Docker. To exercise Docker, add `--require-capability docker` and `--inputs-json '{"docker_image":"sha256:…"}'` with a preloaded image containing `/bin/sh`.

```sh
c2j submit 'Check ordinary worker on the candidate' \
  --jobdb "$JOBDB" --cell "$CELL" --require-base ubuntu-24.04-arm64 \
  --advanced-recipe-file scripts/kvm/plain-job.yaml --json
```

Where the candidate advertises KVM, build the inner test payload from the repo root using Python 3, curl, Go, and an ARM64 C compiler with static libc. Only the VM launched by the job uses the payload’s pinned v55 VMM. The outer VM uses the provider binary, VMM, and guest assets selected by your configuration. Allow the assigned guest enough RAM for a 1,536 MiB inner VM plus its OS and c2j. The offline build accepts `--cache DIR` with the original pinned files `kernel`, `initramfs`, and `cloud-hypervisor`.

```sh
python3 scripts/kvm/qualify.py build /var/tmp/ch-kvm-kit
c2j submit 'Qualify the candidate KVM worker' \
  --jobdb "$JOBDB" --cell "$CELL" --require-base ubuntu-24.04-arm64 \
  --require-capability kvm --advanced-recipe-file scripts/kvm/worker-job.yaml \
  --artifact worker.tar.gz=/var/tmp/ch-kvm-kit/worker.tar.gz --json
```

The recipe first opens KVM and creates a VM as UID/GID 1000, then starts three independent L2 VMs. Each must complete direct PCI virtio disk reads, writes, syncs and readbacks before the deadline. For the networking run add `--inputs-json '{"egress_url":"http://your-test-host.example/probe"}'` using a reachable DNS name that returns HTTP 200 without redirects or credentials. This starts two L2s on dynamic TAPs. One runs in an independently supervised user service. The other must reach the peer and L1 HTTP servers, resolve the external hostname through the advertised DNS server, and fetch the endpoint. For the combined run also request `--require-capability docker` and set `docker_image` in the same JSON to a preloaded `sha256:…` image containing `/bin/sh`. This also checks access to the Docker engine and passed-through KVM device from a user service.

```sh
c2j job result "$JOB_ID" --jobdb "$JOBDB" --field /exit_code --output -
c2j job result "$JOB_ID" --jobdb "$JOBDB" --field /observation --output -
python3 scripts/kvm/qualify.py match "$STATE_DIR" "$LAUNCH" "$IDENTITY"
```

Require exit code zero, three disk passes, and the networking pass when requested. Copy `launch` and `identity` from the worker output into the last command and run it on L0 as the provider owner. It binds that result to the chosen pool’s live record or terminal fence. This distinguishes a job serviced by another KVM-capable worker. On failure inspect the command task’s stderr with `c2j job log`. Keep the worker result, catalog output and identity match together as the qualification record. On the unsupported host, match the ordinary/Docker launch identity the same way and retain its result with the negative catalog result.

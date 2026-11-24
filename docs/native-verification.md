# Verify one candidate on native ARM64 and AMD64 with KVM

Release acceptance requires a native ARM64 run and a native AMD64 run of the same candidate. The current plan has 66 expectations and 22 derived case IDs after retirement of the provider-host reboot test. Publication remains deferred.

`bash scripts/verify-local.sh` is the explicit development command. It needs
native ARM64 or AMD64 Linux and installed tools, but no KVM access, VM assets, networking,
lease service, supervisor authority or fixed large host reservation. It runs
and validates every shared local check and all 78 local mappings, then prints
`VM execution was not tested by this command` on success or failure. The local
path does not read VM fixture or pin inputs. Its temporary receipts and builds
are private and outside the checkout. Unsupported modes fail; environment flags
cannot downgrade the full wrapper or hook to local success.

On an ARM64 development host with QEMU user emulation, the following command executes the AMD64 provider, CLI, guest agent, runner, and state package tests. Change the emulator path to the installed executable.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test \
  -exec /usr/bin/qemu-x86_64-static -count=1 \
  . ./cmd/pulse-cloudhypervisor ./internal/guest ./internal/runner ./internal/state
```

The full-system QEMU test on ARM64 booted the pinned AMD64 Ubuntu image with one vCPU and 1,280 MiB of configured RAM. The candidate agent handled preparation and started the pinned-source AMD64 c2j as the worker. Its isolated JobDB fixture verified the job outcome and exact stdout and stderr. Native AMD64 KVM and nested virtualization have separate hardware checks. [Emulated AMD64 guest run](http://db.lab.c2.run:8081/project/c2/jobs/3MZaqeSPp7jxxpM3wYIsXJ1HekD/story)

The repository selects `ubuntu*`, `kvm` and the approved hypervisor package
without universal CPU, memory or scratch minima. VM preflight checks actual capacity against the selected
concurrent guest/builder workload when it runs. No special instance class or
separate certification process is required.

A native Linux/KVM worker can run host tests without a guest fixture. With
the installed Go toolchain on PATH, run:

```sh
GOWORK=off GOTOOLCHAIN=local go test -count=1 -tags=live -run='^TestNativeHost$' -timeout=30s -v ./tests/live
```

This test opens KVM, checks API version 12, creates and closes an empty KVM VM,
then starts the installed Cloud Hypervisor with a private Unix API socket,
checks its ping response and shuts down that exact process. It uses no guest
image, lease, network changes or systemd service. Cleanup is restricted to its
owned descriptors, process and temporary directory. Missing KVM or VMM access
fails the test. Passing it demonstrates these host operations; guest boot and
the ten LIVE requirements still need their complete assertions and inputs.

Stage Ubuntu boot source assets without root or installing host packages:

```sh
python3 scripts/native_assets.py --output /absolute/new/private-assets
```

This downloads the matching architecture from the fixed Ubuntu 24.04 release from 17 October 2026 and
checks the recorded official SHA-256 values before using it. The UEC archive's
bare ext4 filesystem is wrapped in deterministic GPT partition 1 for `/dev/vda1`.
It writes a sparse raw disk, an ARM64 Image or AMD64 bzImage kernel, the initramfs and `assets.json`
with measured paths/hashes. It refuses an existing destination and never extracts
archive paths, links or device nodes. The full hook then calls `scripts/native_setup.py` to append the candidate
native agent, service and builder-only installer. The initramfs mounts the
verified GPT partition, checks its device number and starts Ubuntu userspace.
The installer requires Ubuntu 24.04 and creates the UID/GID 1000 worker layout.
A lease-free ARM64/KVM boot reached the root filesystem shell and reported
`ubuntu/24.04 aarch64`. The builder must separately return its tool and Docker
proof before the provider can promote that source. [Root boot recipe](http://db.lab.c2.run:8081/project/c2/jobs/3MUyeMC02txeevmCfK65rAQ7Yw6/story)

The runner explicitly supplies `image_type=raw` for builder and job disks;
the approved Cloud Hypervisor v55 rejects a disk argument that omits the type.

For allocations supplied by pulse-macvz, follow its
[Linux/KVM instructions](https://github.com/colony-2/pulse-macvz/blob/fcfcf56a47830f573e8045e54ae36e29892351fd/docs/linux-kvm.md).
The documented route provides a network descriptor at `PULSE_KVM_NETWORK`, a
lingering user manager with explicit runtime/bus environment and the
`pulse-kvm-run` capability launcher for independent user services. Check that
these exist on this actual allocation before using them. Published source or an
older readiness receipt does not prove rollout here. Do not run preparation on
an accepted allocation or bypass its launch-receipt guard. A failed handoff needs
owner rollout/access correction, rather than another implementation of the same
published fix. No parent Mac or unrelated allocation is in scope.

`bash scripts/verify.sh` runs the unit and race suites, vet, hook tests, external consumers, builds for both architectures, and native host and guest tests on the current worker. The release hook requests separate `ubuntu*-arm64` and `ubuntu*-amd64` workers with KVM. It checks the running architecture and passes the ARM64 receipt to the AMD64 step. The collector checks both receipts against the candidate, source digest, test mapping, asset pins, and the two guest agents rebuilt from that candidate. A missing or mismatched receipt fails release qualification.

The managed hook accepts the default profile from its settings artifact. It builds the candidate binaries, stages private
boot assets, prepares two owned TAPs and starts a disposable JobDB helper once
the builder has stopped and its source has qualified. The helper accepts
guest HTTP requests from the reserved addresses and authenticates host control
requests on a private same-user Unix socket. The hook passes the generated
pins and descriptor directly to acceptance.

| Variable | Role |
| --- | --- |
| `CANDIDATE` | Managed candidate identity. Local checks default to Git HEAD. |
| `CLOUDHYPERVISOR_PINS` | Absolute native-architecture pin collection for a standalone acceptance invocation. |
| `CLOUDHYPERVISOR_FIXTURE` | Absolute private fixture descriptor for that invocation. |

For an independent native run, use `bash scripts/acceptance.sh --arch arm64 --candidate ID --result /absolute/scratch/native.json` on ARM64. Use `--arch amd64` on AMD64. `--result` is optional for a check without an exported receipt. The output file must be new and outside the checkout. Missing inputs or native prerequisites fail. A successful preflight alone cannot produce passing native acceptance. [ARM64 and AMD64 qualification plan](provider-improvements.md#5-run-the-provider-on-both-arm64-and-amd64)

The version-1 pin collection contains `common` and `architectures` objects. Common pins contain `provider_api`, `plugin_sdk`, integer `sdk_baseline`, a full `c2j_revision` and `go_version`. Each native architecture entry contains SHA-256 values for `hypervisor`, `disk`, `kernel`, `preparation` and `bootstrap_agent`; `initramfs` is its SHA-256 or JSON null. `tools` contains separate `c2j`, `nix` and `docker` executable hashes; `docker_image` is a fixed `sha256:` image identity. Unknown fields and incomplete collections fail. The installed candidate guest agent is measured after building and recorded separately, avoiding a self-referential source pin.

For a managed native run, `CH_NATIVE_RESULT` exports a passing receipt to a new path outside the checkout. `CH_OTHER_NATIVE` supplies the other architecture’s receipt for combined qualification. For separate release review, run `scripts/check-coverage.py` with both `--native` files, both `--guest-agent-ARCH` binaries and a pin set containing both architectures. The AMD64 managed fixture checks the native Nix store executables and the pinned c2j source. Deployments can also set `CLOUDHYPERVISOR_AMD64_MANIFEST_SHA256` to check the entire guest tool archive against a reviewed digest.

The private version-1 fixture descriptor contains `identity`, absolute `config` and `control_socket` paths, a `bootstrap_agent` asset (`path`/`sha256`) and declared `faults`. It contains no lease or raw assignment. Common native preflight checks resources for concurrent guests/builders, KVM API access, asset hashes, reserved TAPs, a protected same-user fixture socket and controller/runner fault declarations. Its disposable supervisor probe checks independent launch, inspection and runtime-property changes, with same-user execution, noninteractive commands and cleanup restricted to its exact description. It creates no persistent services.

The [private lease helper](../tests/fixture/README.md) creates valid, expired,
mismatched and completed assignments. Its status response derives job results
and lease renewals from JobDB. The native suite exercises supplied leases,
guest-local Docker, allocation reporting and renewal, concurrent writable
disks, controller and SDK loss, uncertain delivery, listener cancellation,
runtime expiry and cleanup with incomplete process information. Guest cases
require a qualified builder and a fresh native receipt. On a deployed Ubuntu
ARM64 worker with all three GICv3 trap arguments, the disk reproducer
completed three fresh boots. The supervised builder promoted an immutable
Ubuntu source. Independent guests ran Docker as UID 1000 and renewed the
supplied assignment beyond its initial expiry.
[Disk reproducer](http://db.lab.c2.run:8081/project/c2/jobs/3MWMtI6z8QtY3S5BPWDzAbnG4sH/story)
· [Builder qualification](http://db.lab.c2.run:8081/project/c2/jobs/3MWPG4lYwS17CS1xGyVr3kM3efD/story)
· [Docker and lease renewal](http://db.lab.c2.run:8081/project/c2/jobs/3MWRVAwuLemKgzUPTdW2rGHPSCz/story)

The managed verifier binds each run to its JobDB task, acquisition, worker and boot. Receipts bind candidate, source, mapping, complete pins, selected native assets, fixture identity, built guest-agent digest and test command outcome. Source binding includes sorted tracked/nonignored paths, file types, modes and contents, symlink targets and missing tracked files. Git metadata and ignored outputs do not affect it. A dirty source change invalidates evidence even when HEAD is unchanged; source is checked before and after execution.

The bounded event collector discards test output and subprocess stderr. Full native acceptance additionally audits unmapped events, package/test completion, and exit agreement. `error/native-outcomes.json` reports complete, damaged, untrusted, or not-run collection. Only a complete stream with a matching receipt and run binding can retain mapped outcomes; independent passes can remain visible without granting strict acceptance. Unknown result versions, duplicate keys, missing mappings, changed pins, skips, failures, and stale source still fail aggregation. Synthetic events exercise the verifier only.

Lease bytes must arrive in memory through the authorized fixture connection and retain the production Unix/vsock-to-c2j-stdin path. Never supply them through these variables, configuration, files, argv, systemd properties or review evidence. c2j owns validation/renewal; these tools do not acquire a replacement lease or use the active task's lease.

The pinned UEC root has 40,957 free 4 KiB blocks (about 160 MiB). The managed
profile stages a 2.375 GiB partition and expands ext4 inside the disposable
builder. The worker checks a fixed SHA-256 manifest of every file, including
its mode, size and content. The builder verifies the transmitted archive digest
and streams the Nix closures into the store. The prepared source occupies one
disk reservation and each concurrent guest
needs its own writable reservation. [Root layout measurement](http://db.lab.c2.run:8081/project/c2/jobs/3MV5RwH7eqhEuOYS1vGq8HsnOD3/story)

Ubuntu systemd 255 rejected changes to the running service's `RuntimeMaxUSec`.
The ARM64 supervision test verified that preparation can outlast the runtime
cap and that the same production readiness function starts the cap just before
dispatch. A separate exact-unit test killed a synthetic runner and observed its
child through cap expiry. [Readiness test](http://db.lab.c2.run:8081/project/c2/jobs/3MV8dTZCaQsoaoAgsKdHLIFPqcl/story)
[Runner-loss test](http://db.lab.c2.run:8081/project/c2/jobs/3MV76qXeGVyFan1fYOAV3gjLSEK/story)
